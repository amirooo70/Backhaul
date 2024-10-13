package transport

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/musix/backhaul/internal/config" // for mode
	"github.com/musix/backhaul/internal/utils"
	"github.com/musix/backhaul/internal/web"
	"github.com/xtaci/smux"

	"github.com/gorilla/websocket"
	"github.com/sirupsen/logrus"
)

type WsMuxTransport struct {
	config         *WsMuxConfig
	smuxConfig     *smux.Config
	parentctx      context.Context
	ctx            context.Context
	cancel         context.CancelFunc
	logger         *logrus.Logger
	tunnelChannel  chan *smux.Session
	localChannel   chan LocalTCPConn
	reqNewConnChan chan struct{}
	controlChannel *websocket.Conn
	usageMonitor   *web.Usage
	restartMutex   sync.Mutex
}

type WsMuxConfig struct {
	BindAddr         string
	Token            string
	SnifferLog       string
	TLSCertFile      string // Path to the TLS certificate file
	TLSKeyFile       string // Path to the TLS key file
	TunnelStatus     string
	Ports            []string
	Nodelay          bool
	Sniffer          bool
	KeepAlive        time.Duration
	Heartbeat        time.Duration // in seconds
	ChannelSize      int
	MuxCon           int
	MuxVersion       int
	MaxFrameSize     int
	MaxReceiveBuffer int
	MaxStreamBuffer  int
	WebPort          int
	Mode             config.TransportType // ws or wss

}

func NewWSMuxServer(parentCtx context.Context, config *WsMuxConfig, logger *logrus.Logger) *WsMuxTransport {
	// Create a derived context from the parent context
	ctx, cancel := context.WithCancel(parentCtx)

	// Initialize the TcpTransport struct
	server := &WsMuxTransport{
		smuxConfig: &smux.Config{
			Version:           config.MuxVersion,
			KeepAliveInterval: 20 * time.Second,
			KeepAliveTimeout:  40 * time.Second,
			MaxFrameSize:      config.MaxFrameSize,
			MaxReceiveBuffer:  config.MaxReceiveBuffer,
			MaxStreamBuffer:   config.MaxStreamBuffer,
		},
		config:         config,
		parentctx:      parentCtx,
		ctx:            ctx,
		cancel:         cancel,
		logger:         logger,
		tunnelChannel:  make(chan *smux.Session, config.ChannelSize),
		localChannel:   make(chan LocalTCPConn, config.ChannelSize),
		reqNewConnChan: make(chan struct{}, config.ChannelSize),
		controlChannel: nil, // will be set when a control connection is established
		usageMonitor:   web.NewDataStore(fmt.Sprintf(":%v", config.WebPort), ctx, config.SnifferLog, config.Sniffer, &config.TunnelStatus, logger),
	}

	return server
}

func (s *WsMuxTransport) Start() {
	// for  webui
	if s.config.WebPort > 0 {
		go s.usageMonitor.Monitor()
	}

	s.config.TunnelStatus = fmt.Sprintf("Disconnected (%s)", s.config.Mode)

	go s.tunnelListener()

}

func (s *WsMuxTransport) Restart() {
	if !s.restartMutex.TryLock() {
		s.logger.Warn("server restart already in progress, skipping restart attempt")
		return
	}
	defer s.restartMutex.Unlock()

	s.logger.Info("restarting server...")
	if s.cancel != nil {
		s.cancel()
	}

	// Close tunnel channel connection
	if s.controlChannel != nil {
		s.controlChannel.Close()
	}

	time.Sleep(2 * time.Second)

	ctx, cancel := context.WithCancel(s.parentctx)
	s.ctx = ctx
	s.cancel = cancel

	// Re-initialize variables
	s.tunnelChannel = make(chan *smux.Session, s.config.ChannelSize)
	s.localChannel = make(chan LocalTCPConn, s.config.ChannelSize)
	s.reqNewConnChan = make(chan struct{}, s.config.ChannelSize)
	s.controlChannel = nil
	s.usageMonitor = web.NewDataStore(fmt.Sprintf(":%v", s.config.WebPort), ctx, s.config.SnifferLog, s.config.Sniffer, &s.config.TunnelStatus, s.logger)
	s.config.TunnelStatus = ""

	go s.Start()

}

func (s *WsMuxTransport) channelHandler() {
	ticker := time.NewTicker(s.config.Heartbeat)
	defer ticker.Stop()

	// Channel to receive the message or error
	resultChan := make(chan struct {
		message []byte
		err     error
	})
	go func() {
		_, message, err := s.controlChannel.ReadMessage()
		resultChan <- struct {
			message []byte
			err     error
		}{message, err}
	}()

	for {
		select {
		case <-s.ctx.Done():
			_ = s.controlChannel.WriteMessage(websocket.BinaryMessage, []byte{utils.SG_Closed})
			return
		case <-s.reqNewConnChan:
			err := s.controlChannel.WriteMessage(websocket.BinaryMessage, []byte{utils.SG_Chan})
			if err != nil {
				s.logger.Error("error sending channel signal, attempting to restart server...")
				go s.Restart()
				return
			}

		case <-ticker.C:
			if s.controlChannel == nil {
				s.logger.Warn("control channel is nil. Restarting server to re-establish connection...")
				go s.Restart()
				return
			}
			err := s.controlChannel.WriteMessage(websocket.BinaryMessage, []byte{utils.SG_HB})
			if err != nil {
				s.logger.Errorf("Failed to send heartbeat signal. Error: %v. Restarting server...", err)
				go s.Restart()
				return
			}
			s.logger.Debug("heartbeat signal sent successfully")

		case result := <-resultChan:
			if result.err != nil {
				s.logger.Errorf("failed to receive message from channel connection: %v", result.err)
				go s.Restart()
				return
			}
			if bytes.Equal(result.message, []byte{utils.SG_Closed}) {
				s.logger.Info("control channel has been closed by the client")
				go s.Restart()
				return
			}
		}
	}
}

func (s *WsMuxTransport) tunnelListener() {
	addr := s.config.BindAddr
	upgrader := websocket.Upgrader{
		ReadBufferSize:  16 * 1024,
		WriteBufferSize: 16 * 1024,
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
	}

	// Create an HTTP server
	server := &http.Server{
		Addr:        addr,
		IdleTimeout: 600 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			s.logger.Tracef("received http request from %s", r.RemoteAddr)

			// Read the "Authorization" header
			authHeader := r.Header.Get("Authorization")
			if authHeader != fmt.Sprintf("Bearer %v", s.config.Token) {
				s.logger.Warnf("unauthorized request from %s, closing connection", r.RemoteAddr)
				http.Error(w, "unauthorized", http.StatusUnauthorized) // Send 401 Unauthorized response
				return
			}

			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				s.logger.Errorf("failed to upgrade connection from %s: %v", r.RemoteAddr, err)
				return
			}

			if r.URL.Path == "/channel" && s.controlChannel == nil {
				s.controlChannel = conn

				s.logger.Info("control channel established successfully")

				numCPU := runtime.NumCPU()
				if numCPU > 4 {
					numCPU = 4 // Max allowed handler is 4
				}

				go s.channelHandler()
				go s.parsePortMappings()

				s.logger.Infof("starting %d handle loops on each CPU thread", numCPU)

				for i := 0; i < numCPU; i++ {
					go s.handleLoop()
				}

				s.config.TunnelStatus = fmt.Sprintf("Connected (%s)", s.config.Mode)

			} else if r.URL.Path == "/tunnel" {
				session, err := smux.Client(conn.NetConn(), s.smuxConfig)
				if err != nil {
					s.logger.Errorf("failed to create MUX session for connection %s: %v", conn.RemoteAddr().String(), err)
					conn.Close()
					return
				}
				select {
				case s.tunnelChannel <- session: // ok
				default:
					s.logger.Warnf("tunnel listener channel is full, discarding TCP connection from %s", conn.LocalAddr().String())
					conn.Close()
				}
			}
		}),
	}

	if s.config.Mode == config.WSMUX {
		go func() {
			s.logger.Infof("%s server starting, listening on %s", s.config.Mode, addr)
			if s.controlChannel == nil {
				s.logger.Infof("waiting for %s control channel connection", s.config.Mode)
			}
			if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				s.logger.Fatalf("failed to listen on %s: %v", addr, err)
			}
		}()
	} else {
		go func() {
			s.logger.Infof("%s server starting, listening on %s", s.config.Mode, addr)
			if s.controlChannel == nil {
				s.logger.Infof("waiting for %s control channel connection", s.config.Mode)
			}
			if err := server.ListenAndServeTLS(s.config.TLSCertFile, s.config.TLSKeyFile); err != nil && err != http.ErrServerClosed {
				s.logger.Fatalf("failed to listen on %s: %v", addr, err)
			}
		}()
	}

	<-s.ctx.Done()

	// close connection
	if s.controlChannel != nil {
		s.controlChannel.Close()
	}

	// Gracefully shutdown the server
	s.logger.Infof("shutting down the websocket server on %s", addr)
	if err := server.Shutdown(context.Background()); err != nil {
		s.logger.Errorf("Failed to gracefully shutdown the server: %v", err)
	}
}

func (s *WsMuxTransport) parsePortMappings() {
	// port mapping for listening on each local port
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
	}
}

func (s *WsMuxTransport) localListener(localAddr string, remoteAddr string) {
	listener, err := net.Listen("tcp", localAddr)
	if err != nil {
		s.logger.Fatalf("failed to start listener on %s: %v", localAddr, err)
		return
	}

	//close local listener after context cancellation
	defer listener.Close()

	go s.acceptLocalConn(listener, remoteAddr)

	s.logger.Infof("listener started successfully, listening on address: %s", listener.Addr().String())

	<-s.ctx.Done()
}

func (s *WsMuxTransport) acceptLocalConn(listener net.Listener, remoteAddr string) {
	for {
		select {
		case <-s.ctx.Done():
			return

		default:
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

			// trying to enable tcpnodelay
			if !s.config.Nodelay {
				if err := tcpConn.SetNoDelay(s.config.Nodelay); err != nil {
					s.logger.Warnf("failed to set TCP_NODELAY for %s: %v", tcpConn.RemoteAddr().String(), err)
				} else {
					s.logger.Tracef("TCP_NODELAY disabled for %s", tcpConn.RemoteAddr().String())
				}
			}

			select {
			case s.localChannel <- LocalTCPConn{conn: conn, remoteAddr: remoteAddr}:
				s.logger.Debugf("accepted incoming TCP connection from %s", tcpConn.RemoteAddr().String())

			default: // channel is full, discard the connection
				s.logger.Warnf("local listener channel is full, discarding TCP connection from %s", tcpConn.LocalAddr().String())
				conn.Close()
			}
		}
	}

}

func (s *WsMuxTransport) handleLoop() {
	next := make(chan struct{})
	for {
		select {
		case <-s.ctx.Done():
			return

		case tunConn := <-s.tunnelChannel:
			go s.handleSession(tunConn, next)
			<-next
		}
	}
}

func (s *WsMuxTransport) handleSession(session *smux.Session, next chan struct{}) {
	done := make(chan struct{}, s.config.MuxCon)
	counter := 0

	for {
		select {
		case <-s.ctx.Done():
			for counter > 0 { // Ensure all goroutines finish before returning
				<-done
				counter--
			}
			return

		case incomingConn := <-s.localChannel:
			stream, err := session.OpenStream()
			if err != nil {
				s.handleSessionError(session, &incomingConn, next, done, counter, err)
				return
			}
			// In my tests, sending the data frame immediately after opening the stream caused SMUX to misinterpret the header in WebSocket mode.
			// To prevent this issue, a short delay of 1ms is introduced after opening the stream
			// before sending any data. This ensures proper header parsing by SMUX.
			time.Sleep(1 * time.Millisecond)

			// Send the target port over the tunnel connection
			if err := utils.SendBinaryString(stream, incomingConn.remoteAddr); err != nil {
				s.handleSessionError(session, &incomingConn, next, done, counter, err)
				return
			}

			// Handle data exchange between connections
			go func() {
				utils.TCPConnectionHandler(stream, incomingConn.conn, s.logger, s.usageMonitor, incomingConn.conn.LocalAddr().(*net.TCPAddr).Port, s.config.Sniffer)
				done <- struct{}{}
			}()

			counter++

			// Check if the maximum number of multiplexed connections is reached
			if counter == s.config.MuxCon {
				s.finalizeSession(session, next, done, counter)
				return
			}
		}
	}
}

func (s *WsMuxTransport) handleSessionError(session *smux.Session, incomingConn *LocalTCPConn, next chan struct{}, done chan struct{}, counter int, err error) {
	s.logger.Errorf("failed to handle session: %v", err)

	// Put connection back to local channel
	s.localChannel <- *incomingConn

	// Notify to start a new session
	next <- struct{}{}

	// Attempt to request a new connection
	select {
	case s.reqNewConnChan <- struct{}{}:
	default:
		s.logger.Warn("request new connection channel is full")
	}

	// Wait for all active handlers to finish
	for i := 0; i < counter; i++ {
		<-done
	}

	// Ensure session is closed
	if closeErr := session.Close(); closeErr != nil {
		s.logger.Errorf("failed to close session: %v", closeErr)
	}
}

func (s *WsMuxTransport) finalizeSession(session *smux.Session, next chan struct{}, done chan struct{}, counter int) {
	next <- struct{}{}

	// Attempt to request a new connection
	select {
	case s.reqNewConnChan <- struct{}{}:
	default:
		s.logger.Warn("request new connection channel is full")
	}

	// Wait for all active handlers to finish
	for i := 0; i < counter; i++ {
		<-done
	}

	// Ensure session is closed after completing the mux session
	if err := session.Close(); err != nil {
		s.logger.Errorf("failed to close session after session completed: %v", err)
	}
}
