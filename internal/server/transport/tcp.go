package transport

import (
	"context"
	"fmt"
	"net"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/musix/backhaul/internal/utils"
	"github.com/musix/backhaul/internal/web"

	"github.com/sirupsen/logrus"
)

type TcpTransport struct {
	config         *TcpConfig
	parentctx      context.Context
	ctx            context.Context
	cancel         context.CancelFunc
	logger         *logrus.Logger
	tunnelChannel  chan net.Conn
	localChannel   chan LocalTCPConn
	reqNewConnChan chan struct{}
	controlChannel net.Conn
	restartMutex   sync.Mutex
	usageMonitor   *web.Usage
}

type TcpConfig struct {
	BindAddr     string
	Token        string
	SnifferLog   string
	TunnelStatus string
	Ports        []string
	Nodelay      bool
	Sniffer      bool
	KeepAlive    time.Duration
	Heartbeat    time.Duration // in seconds
	ChannelSize  int
	WebPort      int
	AcceptUDP    bool
}

func NewTCPServer(parentCtx context.Context, config *TcpConfig, logger *logrus.Logger) *TcpTransport {
	// Create a derived context from the parent context
	ctx, cancel := context.WithCancel(parentCtx)

	// Initialize the TcpTransport struct
	server := &TcpTransport{
		config:         config,
		parentctx:      parentCtx,
		ctx:            ctx,
		cancel:         cancel,
		logger:         logger,
		tunnelChannel:  make(chan net.Conn, config.ChannelSize),
		localChannel:   make(chan LocalTCPConn, config.ChannelSize),
		reqNewConnChan: make(chan struct{}, config.ChannelSize),
		controlChannel: nil, // will be set when a control connection is established
		usageMonitor:   web.NewDataStore(fmt.Sprintf(":%v", config.WebPort), ctx, config.SnifferLog, config.Sniffer, &config.TunnelStatus, logger),
	}

	return server
}

func (s *TcpTransport) Start() {
	s.config.TunnelStatus = "Disconnected (TCP)"

	if s.config.WebPort > 0 {
		go s.usageMonitor.Monitor()
	}

	go s.tunnelListener()

	s.channelHandshake()

	if s.controlChannel != nil {
		s.config.TunnelStatus = "Connected (TCP)"

		numCPU := runtime.NumCPU()
		if numCPU > 4 {
			numCPU = 4 // Max allowed handler is 4
		}

		go s.parsePortMappings()
		go s.channelHandler()

		s.logger.Infof("starting %d handle loops on each CPU thread", numCPU)

		for i := 0; i < numCPU; i++ {
			go s.handleLoop()
		}
	}
}
func (s *TcpTransport) Restart() {
	if !s.restartMutex.TryLock() {
		s.logger.Warn("server restart already in progress, skipping restart attempt")
		return
	}
	defer s.restartMutex.Unlock()

	s.logger.Info("restarting server...")
	if s.cancel != nil {
		s.cancel()
	}

	// Close open connection
	if s.controlChannel != nil {
		s.controlChannel.Close()
	}

	time.Sleep(2 * time.Second)

	ctx, cancel := context.WithCancel(s.parentctx)
	s.ctx = ctx
	s.cancel = cancel

	// Re-initialize variables
	s.tunnelChannel = make(chan net.Conn, s.config.ChannelSize)
	s.localChannel = make(chan LocalTCPConn, s.config.ChannelSize)
	s.reqNewConnChan = make(chan struct{}, s.config.ChannelSize)
	s.usageMonitor = web.NewDataStore(fmt.Sprintf(":%v", s.config.WebPort), ctx, s.config.SnifferLog, s.config.Sniffer, &s.config.TunnelStatus, s.logger)
	s.config.TunnelStatus = ""
	s.controlChannel = nil

	go s.Start()

}

func (s *TcpTransport) channelHandshake() {
	for {
		select {
		case <-s.ctx.Done():
			return
		case conn := <-s.tunnelChannel:
			// Set a read deadline for the token response
			if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
				s.logger.Errorf("failed to set read deadline: %v", err)
				conn.Close()
				continue
			}

			msg, transport, err := utils.ReceiveBinaryTransportString(conn)
			if transport != utils.SG_Chan {
				s.logger.Errorf("invalid signal received for channel, Discarding connection")
				conn.Close()
				continue
			} else if err != nil {
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					s.logger.Warn("timeout while waiting for control channel signal")
				} else {
					s.logger.Errorf("failed to receive control channel signal: %v", err)
				}
				conn.Close() // Close connection on error or timeout
				continue
			}

			// Resetting the deadline (removes any existing deadline)
			conn.SetReadDeadline(time.Time{})

			if msg != s.config.Token {
				s.logger.Warnf("invalid security token received: %s", msg)
				conn.Close()
				continue
			}

			err = utils.SendBinaryTransportString(conn, s.config.Token, utils.SG_Chan)
			if err != nil {
				s.logger.Errorf("failed to send security token: %v", err)
				conn.Close()
				continue
			}

			s.controlChannel = conn

			s.logger.Info("control channel successfully established.")
			return
		}
	}
}

func (s *TcpTransport) channelHandler() {
	ticker := time.NewTicker(s.config.Heartbeat)
	defer ticker.Stop()

	// Channel to receive the message or error
	resultChan := make(chan struct {
		message byte
		err     error
	})

	go func() {
		message, err := utils.ReceiveBinaryByte(s.controlChannel)
		resultChan <- struct {
			message byte
			err     error
		}{message, err}
	}()

	for {
		select {
		case <-s.ctx.Done():
			_ = utils.SendBinaryByte(s.controlChannel, utils.SG_Closed)
			return
		case <-s.reqNewConnChan:
			err := utils.SendBinaryByte(s.controlChannel, utils.SG_Chan)
			if err != nil {
				s.logger.Error("error sending channel signal, attempting to restart server...")
				go s.Restart()
				return
			}
		case <-ticker.C:
			err := utils.SendBinaryByte(s.controlChannel, utils.SG_HB)
			if err != nil {
				s.logger.Error("failed to send heartbeat signal, attempting to restart server...")
				go s.Restart()
				return
			}
			s.logger.Trace("heartbeat signal sent successfully")

		case result := <-resultChan:
			if result.err != nil {
				s.logger.Errorf("failed to receive message from channel connection: %v", result.err)
				go s.Restart()
				return
			}
			if result.message == utils.SG_Closed {
				s.logger.Info("control channel has been closed by the client")
				go s.Restart()
				return
			}
		}
	}
}

func (s *TcpTransport) tunnelListener() {
	listener, err := net.Listen("tcp", s.config.BindAddr)
	if err != nil {
		s.logger.Fatalf("failed to start listener on %s: %v", s.config.BindAddr, err)
		return
	}

	defer listener.Close()

	s.logger.Infof("server started successfully, listening on address: %s", listener.Addr().String())

	go s.acceptTunnelConn(listener)

	<-s.ctx.Done()
}

func (s *TcpTransport) acceptTunnelConn(listener net.Listener) {
	for {
		select {
		case <-s.ctx.Done():
			return
		default:
			s.logger.Debugf("waiting for accept incoming tunnel connection on %s", listener.Addr().String())
			conn, err := listener.Accept()
			if err != nil {
				s.logger.Debugf("failed to accept tunnel connection on %s: %v", listener.Addr().String(), err)
				continue
			}

			//discard any non tcp connection
			tcpConn, ok := conn.(*net.TCPConn)
			if !ok {
				s.logger.Warnf("disarded non-TCP tunnel connection from %s", conn.RemoteAddr().String())
				conn.Close()
				continue
			}

			// Drop all suspicious packets from other address rather than server
			if s.controlChannel != nil && s.controlChannel.RemoteAddr().(*net.TCPAddr).IP.String() != tcpConn.RemoteAddr().(*net.TCPAddr).IP.String() {
				s.logger.Debugf("suspicious packet from %v. expected address: %v. discarding packet...", tcpConn.RemoteAddr().(*net.TCPAddr).IP.String(), s.controlChannel.RemoteAddr().(*net.TCPAddr).IP.String())
				tcpConn.Close()
				continue
			}

			// trying to set tcpnodelay
			if !s.config.Nodelay {
				if err := tcpConn.SetNoDelay(s.config.Nodelay); err != nil {
					s.logger.Warnf("failed to set TCP_NODELAY for %s: %v", tcpConn.RemoteAddr().String(), err)
				} else {
					s.logger.Tracef("TCP_NODELAY disabled for %s", tcpConn.RemoteAddr().String())
				}
			}

			// Set keep-alive settings
			if err := tcpConn.SetKeepAlive(true); err != nil {
				s.logger.Warnf("failed to enable TCP keep-alive for %s: %v", tcpConn.RemoteAddr().String(), err)
			} else {
				s.logger.Tracef("TCP keep-alive enabled for %s", tcpConn.RemoteAddr().String())
			}
			if err := tcpConn.SetKeepAlivePeriod(s.config.KeepAlive); err != nil {
				s.logger.Warnf("failed to set TCP keep-alive period for %s: %v", tcpConn.RemoteAddr().String(), err)
			}

			select {
			case s.tunnelChannel <- conn:
			default: // The channel is full, do nothing
				s.logger.Warnf("tunnel listener channel is full, discarding TCP connection from %s", conn.LocalAddr().String())
				conn.Close()
			}
		}
	}
}

func (s *TcpTransport) parsePortMappings() {
	for _, portMapping := range s.config.Ports {
		var localAddr string
		parts := strings.Split(portMapping, "=")
		if len(parts) < 2 {
			port, err := strconv.Atoi(parts[0])
			if err != nil {
				s.logger.Fatalf("invalid port mapping format: %s", portMapping)
			}
			localAddr = fmt.Sprintf(":%d", port)
			parts = append(parts, strconv.Itoa(port))
		} else {
			localAddr = strings.TrimSpace(parts[0])
			if _, err := strconv.Atoi(localAddr); err == nil {
				localAddr = ":" + localAddr // :3080 format
			}
		}

		remoteAddr := strings.TrimSpace(parts[1])

		go s.localListener(localAddr, remoteAddr)

		if s.config.AcceptUDP {
			go s.udpListener(localAddr, remoteAddr)
		}
	}
}

func (s *TcpTransport) localListener(localAddr string, remoteAddr string) {
	listener, err := net.Listen("tcp", localAddr)
	if err != nil {
		s.logger.Fatalf("failed to listen on %s: %v", localAddr, err)
		return
	}

	defer listener.Close()

	s.logger.Infof("listener started successfully, listening on address: %s", listener.Addr().String())

	go s.acceptLocalConn(listener, remoteAddr)

	<-s.ctx.Done()
}

func (s *TcpTransport) acceptLocalConn(listener net.Listener, remoteAddr string) {
	for {
		select {
		case <-s.ctx.Done():
			return

		default:
			s.logger.Debugf("waiting for accept incoming connection on %s", listener.Addr().String())
			conn, err := listener.Accept()
			if err != nil {
				s.logger.Debugf("failed to accept connection on %s: %v", listener.Addr().String(), err)
				continue
			}

			// discard any non-tcp connection
			tcpConn, ok := conn.(*net.TCPConn)
			if !ok {
				s.logger.Warnf("disarded non-TCP connection from %s", conn.RemoteAddr().String())
				conn.Close()
				continue
			}

			// trying to disable tcpnodelay
			if !s.config.Nodelay {
				if err := tcpConn.SetNoDelay(s.config.Nodelay); err != nil {
					s.logger.Warnf("failed to set TCP_NODELAY for %s: %v", tcpConn.RemoteAddr().String(), err)
				} else {
					s.logger.Tracef("TCP_NODELAY disabled for %s", tcpConn.RemoteAddr().String())
				}
			}

			select {
			case s.localChannel <- LocalTCPConn{conn: conn, remoteAddr: remoteAddr}:

				select {
				case s.reqNewConnChan <- struct{}{}:
					// Successfully requested a new connection
				default:
					// The channel is full, do nothing
					s.logger.Warn("channel is full, cannot request a new connection")
				}

				s.logger.Debugf("accepted incoming TCP connection from %s", tcpConn.RemoteAddr().String())

			default: // channel is full, discard the connection
				s.logger.Warnf("channel with listener %s is full, discarding TCP connection from %s", listener.Addr().String(), tcpConn.LocalAddr().String())
				conn.Close()
			}
		}
	}
}

func (s *TcpTransport) handleLoop() {
	for {
		select {
		case <-s.ctx.Done():
			return
		case localConn := <-s.localChannel:
		loop:
			for {
				select {
				case <-s.ctx.Done():
					return

				case tunnelConn := <-s.tunnelChannel:
					// Send the target addr over the connection
					if err := utils.SendBinaryTransportString(tunnelConn, localConn.remoteAddr, utils.SG_TCP); err != nil {
						s.logger.Errorf("%v", err)
						tunnelConn.Close()
						continue loop
					}

					// Handle data exchange between connections
					go utils.TCPConnectionHandler(localConn.conn, tunnelConn, s.logger, s.usageMonitor, localConn.conn.LocalAddr().(*net.TCPAddr).Port, s.config.Sniffer)
					break loop

				}
			}
		}
	}
}
