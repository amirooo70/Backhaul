package transport

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/musix/backhaul/internal/utils"
	"github.com/musix/backhaul/internal/web"

	"github.com/sirupsen/logrus"
)

type TcpTransport struct {
	config          *TcpConfig
	parentctx       context.Context
	ctx             context.Context
	cancel          context.CancelFunc
	logger          *logrus.Logger
	controlChannel  net.Conn
	usageMonitor    *web.Usage
	restartMutex    sync.Mutex
	poolConnections int32
	loadConnections int32
	controlFlow     chan struct{}
}
type TcpConfig struct {
	RemoteAddr    string
	Token         string
	SnifferLog    string
	TunnelStatus  string
	KeepAlive     time.Duration
	RetryInterval time.Duration
	DialTimeOut   time.Duration
	ConnPoolSize  int
	WebPort       int
	Nodelay       bool
	Sniffer       bool
}

func NewTCPClient(parentCtx context.Context, config *TcpConfig, logger *logrus.Logger) *TcpTransport {
	// Create a derived context from the parent context
	ctx, cancel := context.WithCancel(parentCtx)

	// Initialize the TcpTransport struct
	client := &TcpTransport{
		config:          config,
		parentctx:       parentCtx,
		ctx:             ctx,
		cancel:          cancel,
		logger:          logger,
		controlChannel:  nil, // will be set when a control connection is established
		usageMonitor:    web.NewDataStore(fmt.Sprintf(":%v", config.WebPort), ctx, config.SnifferLog, config.Sniffer, &config.TunnelStatus, logger),
		poolConnections: 0,
		loadConnections: 0,
		controlFlow:     make(chan struct{}),
	}

	return client
}

func (c *TcpTransport) Start() {
	if c.config.WebPort > 0 {
		go c.usageMonitor.Monitor()
	}

	c.config.TunnelStatus = "Disconnected (TCP)"

	go c.channelDialer()
}
func (c *TcpTransport) Restart() {
	if !c.restartMutex.TryLock() {
		c.logger.Warn("client is already restarting")
		return
	}
	defer c.restartMutex.Unlock()

	c.logger.Info("restarting client...")
	if c.cancel != nil {
		c.cancel()
	}

	time.Sleep(2 * time.Second)

	ctx, cancel := context.WithCancel(c.parentctx)
	c.ctx = ctx
	c.cancel = cancel

	// Re-initialize variables
	c.controlChannel = nil
	c.usageMonitor = web.NewDataStore(fmt.Sprintf(":%v", c.config.WebPort), ctx, c.config.SnifferLog, c.config.Sniffer, &c.config.TunnelStatus, c.logger)
	c.config.TunnelStatus = ""
	c.poolConnections = 0
	c.loadConnections = 0
	c.controlFlow = make(chan struct{})

	go c.Start()

}

func (c *TcpTransport) channelDialer() {
	c.logger.Info("attempting to establish a new control channel connection...")

	for {
		select {
		case <-c.ctx.Done():
			return
		default:
			tunnelTCPConn, err := TcpDialer(c.config.RemoteAddr, c.config.DialTimeOut, c.config.KeepAlive, c.config.Nodelay)
			if err != nil {
				c.logger.Errorf("channel dialer: error dialing remote address %s: %v", c.config.RemoteAddr, err)
				time.Sleep(c.config.RetryInterval)
				continue
			}

			// Sending security token
			err = utils.SendBinaryString(tunnelTCPConn, c.config.Token)
			if err != nil {
				c.logger.Errorf("failed to send security token: %v", err)
				tunnelTCPConn.Close()
				continue
			}

			// Set a read deadline for the token response
			if err := tunnelTCPConn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
				c.logger.Errorf("failed to set read deadline: %v", err)
				tunnelTCPConn.Close()
				continue
			}

			// Receive response
			message, err := utils.ReceiveBinaryString(tunnelTCPConn)
			if err != nil {
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					c.logger.Warn("timeout while waiting for control channel response")
				} else {
					c.logger.Errorf("failed to receive control channel response: %v", err)
				}
				tunnelTCPConn.Close() // Close connection on error or timeout
				time.Sleep(c.config.RetryInterval)
				continue
			}
			// Resetting the deadline (removes any existing deadline)
			tunnelTCPConn.SetReadDeadline(time.Time{})

			if message == c.config.Token {
				c.controlChannel = tunnelTCPConn
				c.logger.Info("control channel established successfully")

				c.config.TunnelStatus = "Connected (TCP)"
				go c.poolMaintainer()
				go c.channelHandler()

				return

			} else {
				c.logger.Errorf("invalid token received. Expected: %s, Received: %s. Retrying...", c.config.Token, message)
				tunnelTCPConn.Close() // Close connection if the token is invalid
				time.Sleep(c.config.RetryInterval)
				continue
			}
		}
	}
}

func (c *TcpTransport) poolMaintainer() {
	for i := 0; i < c.config.ConnPoolSize; i++ { //initial pool filling
		go c.tunnelDialer()
	}

	tickerPool := time.NewTicker(time.Second * 1)
	defer tickerPool.Stop()

	tickerLoad := time.NewTicker(time.Second * 60)
	defer tickerLoad.Stop()

	newPoolSize := c.config.ConnPoolSize // intial value
	var poolConnectionsSum int32 = 0

	for {
		select {
		case <-c.ctx.Done():
			return

		case <-tickerPool.C:
			// Accumulate pool connections over time (every second)
			atomic.AddInt32(&poolConnectionsSum, atomic.LoadInt32(&c.poolConnections))

		case <-tickerLoad.C:
			// Calculate the loadConnections over the last 30 seconds
			loadConnections := (int(atomic.LoadInt32(&c.loadConnections)) + 59) / 60 // Every 1 second, +59 for ceil-like logic
			atomic.StoreInt32(&c.loadConnections, 0)                                 // Reset

			// Calculate the average pool connections over the last 10 seconds
			poolConnectionsAvg := (int(atomic.LoadInt32(&poolConnectionsSum)) + 59) / 60 // Average connections in 1 second, +59 for ceil-like logic
			atomic.StoreInt32(&poolConnectionsSum, 0)                                    // Reset

			// Dynamically adjust the pool size based on current connections
			if (loadConnections+4)/5 > poolConnectionsAvg { // caclulate in 200ms
				c.logger.Infof("increasing pool size: %d -> %d, avg pool conn: %d, avg load conn: %d", newPoolSize, newPoolSize+1, poolConnectionsAvg, loadConnections)
				newPoolSize++

				// Add a new connection to the pool
				go c.tunnelDialer()
			} else if (loadConnections+3)/4 < poolConnectionsAvg && newPoolSize > c.config.ConnPoolSize { // tolerance for decreasing pool is 20%
				c.logger.Infof("decreasing pool size: %d -> %d", newPoolSize, newPoolSize-1)
				newPoolSize--

				// send a signal to controlFlow
				c.controlFlow <- struct{}{}
			}
		}
	}

}

func (c *TcpTransport) channelHandler() {
	msgChan := make(chan byte, 1000)
	errChan := make(chan error, 1000)

	// Goroutine to handle the blocking ReceiveBinaryString
	go func() {
		for {
			select {
			case <-c.ctx.Done():
				return
			default:
				msg, err := utils.ReceiveBinaryByte(c.controlChannel)
				if err != nil {
					errChan <- err
					return
				}
				msgChan <- msg
			}
		}
	}()

	// Main loop to listen for context cancellation or received messages
	for {
		select {
		case <-c.ctx.Done():
			_ = utils.SendBinaryByte(c.controlChannel, utils.SG_Closed)
			return
		case msg := <-msgChan:
			switch msg {
			case utils.SG_Chan:
				atomic.AddInt32(&c.loadConnections, 1)
				select {
				case <-c.controlFlow: // Do nothing

				default:
					c.logger.Debug("channel signal received, initiating tunnel dialer")
					go c.tunnelDialer()
				}
			case utils.SG_HB:
				c.logger.Debug("heartbeat signal received successfully")
			case utils.SG_Closed:
				c.logger.Info("control channel has been closed by the server")
				go c.Restart()
				return
			default:
				c.logger.Errorf("unexpected response from channel: %v. Restarting client...", msg)
				go c.Restart()
				return
			}
		case err := <-errChan:
			// Handle errors from the control channel
			c.logger.Error("failed to read channel signal, restarting client: ", err)
			go c.Restart()
			return
		}
	}
}

// Dialing to the tunnel server, chained functions, without retry
func (c *TcpTransport) tunnelDialer() {
	c.logger.Debugf("initiating new connection to tunnel server at %s", c.config.RemoteAddr)

	// Dial to the tunnel server
	tcpConn, err := TcpDialer(c.config.RemoteAddr, c.config.DialTimeOut, c.config.KeepAlive, c.config.Nodelay)
	if err != nil {
		c.logger.Error("failed to dial tunnel server: ", err)

		return
	}

	// Increment active connections counter
	atomic.AddInt32(&c.poolConnections, 1)

	// Attempt to receive the remote address from the tunnel server
	remoteAddr, err := utils.ReceiveBinaryString(tcpConn)

	// Decrement active connections after successful or failed connection
	atomic.AddInt32(&c.poolConnections, -1)

	if err != nil {
		c.logger.Debugf("failed to receive port from tunnel connection %s: %v", tcpConn.RemoteAddr().String(), err)
		tcpConn.Close()
		return
	}

	// Extract the port from the received address
	port, resolvedAddr, err := ResolveRemoteAddr(remoteAddr)
	if err != nil {
		c.logger.Infof("failed to resolve remote port: %v", err)
		tcpConn.Close() // Close the connection on error
		return
	}

	// Dial local server using the received address
	c.localDialer(tcpConn, resolvedAddr, port)

}

func (c *TcpTransport) localDialer(tcpConn net.Conn, remoteAddr string, port int) {
	localConnection, err := TcpDialer(remoteAddr, c.config.DialTimeOut, c.config.KeepAlive, c.config.Nodelay)
	if err != nil {
		c.logger.Errorf("failed to connect to local address %s: %v", remoteAddr, err)
		tcpConn.Close()
		return
	}

	c.logger.Debugf("connected to local address %s successfully", remoteAddr)

	utils.TCPConnectionHandler(tcpConn, localConnection, c.logger, c.usageMonitor, port, c.config.Sniffer)
}
