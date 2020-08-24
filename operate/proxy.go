package operate

import (
	"iox/logger"
	"iox/netio"
	"iox/option"
	"iox/socks5"
	"net"
	"os"
	"os/signal"
	"time"
)

func ProxyLocal(local string, encrypted bool) {
	listener, err := net.Listen("tcp", local)
	if err != nil {
		logger.Warn("Socks5 listen on %s error: %s", local, err.Error())
		return
	}

	logger.Success("Start socks5 server on %s", local)

	for {
		conn, err := listener.Accept()
		if err != nil {
			logger.Warn("Socks5 handle local connect error: %s", err.Error())
			continue
		}

		go func() {
			defer conn.Close()
			connCtx, err := netio.NewTCPCtx(conn, encrypted)
			if err != nil {
				return
			}

			socks5.HandleConnection(connCtx)
		}()
	}
}

func ProxyRemote(remote string, encrypted bool) {
	masterConn, err := clientHandshake(remote)
	if err != nil {
		logger.Warn(err.Error())
		return
	}

	connectRequest := make(chan uint8, MAX_CONNECTION/2)
	defer close(connectRequest)

	endSignal := make(chan struct{})

	// handle ctrl+C and send heartbeat packets periodically
	{
		sigs := make(chan os.Signal)
		signal.Notify(sigs, os.Interrupt)
		go func() {
			<-sigs
			masterConn.Write(marshal(Protocol{
				CMD: CTL_CLEANUP,
				N:   0,
			}))
			logger.Success("Recv Ctrl+C, exit now")
			os.Exit(0)
		}()

		ticker := time.NewTicker(time.Second * option.HEARTBEAT_FREQUENCY)
		go func() {
			for {
				<-ticker.C
				masterConn.Write(marshal(Protocol{
					CMD: CTL_HEARTBEAT,
					N:   0,
				}))
			}
		}()
	}

	// handle master conn
	go func() {
		defer masterConn.Close()
		for {
			pb, err := readUntilEnd(masterConn)
			if err != nil {
				continue
			}

			p, err := unmarshal(pb)
			if err != nil {
				continue
			}

			switch p.CMD {
			case CTL_CONNECT_ME:
				connectRequest <- p.N
			case CTL_CLEANUP:
				endSignal <- struct{}{}
				return
			}
		}
	}()

	// handle CONNECT_ME request
	for {
		select {
		case <-endSignal:
			logger.Success("Recv exit signal from remote, exit now")
			return
		case n := <-connectRequest:
			for n > 0 {
				go func() {
					conn, err := net.DialTimeout(
						"tcp", remote,
						time.Duration(option.TIMEOUT)*time.Millisecond,
					)
					if err != nil {
						logger.Info(err.Error())
						return
					}

					connCtx, err := netio.NewTCPCtx(conn, encrypted)
					if err != nil {
						return
					}

					socks5.HandleConnection(connCtx)
				}()
				n--
			}
		}
	}
}

func ProxyRemoteL2L(master string, local string, menc bool, lenc bool) {
	masterListener, err := net.Listen("tcp", master)
	if err != nil {
		logger.Warn("Listen on %s error", master)
		return
	}
	defer masterListener.Close()

	logger.Info("Listen on %s for remote socks5 server", master)

	localListener, err := net.Listen("tcp", local)
	if err != nil {
		logger.Warn("Listen on %s error", local)
		return
	}
	defer localListener.Close()

	masterConn := serverHandshake(masterListener)
	defer func() {
		masterConn.Close()
	}()

	// handle ctrl+C
	{
		sigs := make(chan os.Signal)
		signal.Notify(sigs, os.Interrupt)
		go func() {
			<-sigs
			masterConn.Write(marshal(Protocol{
				CMD: CTL_CLEANUP,
				N:   0,
			}))
			logger.Success("Recv Ctrl+C, exit now")
			os.Exit(0)
		}()
	}

	localConnBuffer := make(chan net.Conn, MAX_CONNECTION/2)
	defer close(localConnBuffer)

	logger.Success("Forward socks5 server to %s", local)

	// handle masterConn read
	go func() {
		for {
			pb, err := readUntilEnd(masterConn)
			if err != nil {
				continue
			}

			p, err := unmarshal(pb)
			if err != nil {
				continue
			}

			switch p.CMD {
			case CTL_CLEANUP:
				logger.Success("Recv exit signal from remote, exit now")
				os.Exit(0)
			case CTL_HEARTBEAT:
				continue
			}
		}
	}()

	// handle local connection
	go func() {
		for {
			localConn, err := localListener.Accept()
			if err != nil {
				continue
			}

			localConnBuffer <- localConn

			masterConn.Write(marshal(Protocol{
				CMD: CTL_CONNECT_ME,
				N:   1,
			}))
		}
	}()

	for {
		remoteConn, err := masterListener.Accept()
		if err != nil {
			continue
		}

		localConn := <-localConnBuffer

		go func() {
			defer remoteConn.Close()
			defer localConn.Close()

			remoteConnCtx, err := netio.NewTCPCtx(remoteConn, menc)
			if err != nil {
				return
			}

			localConnCtx, err := netio.NewTCPCtx(localConn, lenc)
			if err != nil {
				return
			}

			netio.PipeForward(remoteConnCtx, localConnCtx)
		}()
	}
}
