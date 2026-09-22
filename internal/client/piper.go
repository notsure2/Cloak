package client

import (
	"io"
	"net"
	"sync"
	"syscall"
	"time"

	"github.com/cbeuw/Cloak/internal/common"

	mux "github.com/cbeuw/Cloak/internal/multiplex"
	log "github.com/sirupsen/logrus"
)

func RouteUDP(bindFunc func() (*net.UDPConn, error), streamTimeout time.Duration, singleplex bool, newSeshFunc func() *mux.Session) {
	var sesh *mux.Session
	localConn, err := bindFunc()
	if err != nil {
		log.Fatal(err)
	}

	streams := make(map[string]*mux.Stream)
	var streamsMutex sync.Mutex

	// Tear down multiplexed session + all UDP streams so the next packet rebuilds a fresh path.
	// Needed when the outer TLS conn stalls (write deadline / blackhole) — see Cloak#228.
	// Must only be called from the main read/write loop (not from stream reader goroutines).
	resetSession := func(reason string) {
		log.Warnf("resetting UDP session: %s", reason)
		streamsMutex.Lock()
		oldStreams := streams
		streams = make(map[string]*mux.Stream)
		streamsMutex.Unlock()

		for _, st := range oldStreams {
			if st != nil {
				_ = st.Close()
			}
		}
		if sesh != nil && !sesh.IsClosed() {
			sesh.SetTerminalMsg(reason)
			_ = sesh.Close()
		}
		sesh = nil
	}

	data := make([]byte, 8192)
	for {
		i, addr, err := localConn.ReadFrom(data)
		if err != nil {
			log.Errorf("Failed to read first packet from proxy client: %v", err)
			continue
		}

		if !singleplex && (sesh == nil || sesh.IsClosed()) {
			if sesh != nil && sesh.IsClosed() {
				resetSession("session already closed")
			}
			sesh = newSeshFunc()
		}

		streamsMutex.Lock()
		stream, ok := streams[addr.String()]
		if !ok {
			if singleplex {
				sesh = newSeshFunc()
			}

			stream, err = sesh.OpenStream()
			if err != nil {
				log.Errorf("Failed to open stream: %v", err)
				streamsMutex.Unlock()
				if singleplex {
					sesh.Close()
				} else {
					resetSession("failed to open stream: " + err.Error())
				}
				continue
			}
			streams[addr.String()] = stream
			streamsMutex.Unlock()

			_ = stream.SetReadDeadline(time.Now().Add(streamTimeout))

			proxyAddr := addr
			go func(stream *mux.Stream, localConn *net.UDPConn, proxyAddr net.Addr) {
				buf := make([]byte, 8192)
				for {
					n, err := stream.Read(buf)
					if err != nil {
						log.Tracef("copying stream to proxy client: %v", err)
						break
					}
					_ = stream.SetReadDeadline(time.Now().Add(streamTimeout))

					_, err = localConn.WriteTo(buf[:n], proxyAddr)
					if err != nil {
						log.Tracef("copying stream to proxy client: %v", err)
						break
					}
				}
				streamsMutex.Lock()
				delete(streams, proxyAddr.String())
				streamsMutex.Unlock()
				stream.Close()
			}(stream, localConn, proxyAddr)
		} else {
			streamsMutex.Unlock()
		}

		_, err = stream.Write(data[:i])
		if err != nil {
			log.Warnf("copying proxy client to stream: %v", err)
			streamsMutex.Lock()
			delete(streams, addr.String())
			streamsMutex.Unlock()
			stream.Close()
			if !singleplex {
				resetSession("write to remote failed: " + err.Error())
			}
			continue
		}
		_ = stream.SetReadDeadline(time.Now().Add(streamTimeout))
	}
}

func RouteTCP(listener net.Listener, streamTimeout time.Duration, singleplex bool, newSeshFunc func() *mux.Session) {
	var sesh *mux.Session
	for {
		localConn, err := listener.Accept()
		if err != nil {
			log.Fatal(err)
			continue
		}

		conn, ok := localConn.(*net.TCPConn)
		if ok {
			syscallConn, err := conn.SyscallConn()
			if err != nil {
				log.Fatal(err)
				continue
			}

			err = syscallConn.Control(func(fd uintptr) {
				err = syscall.SetsockoptInt(common.Platformfd(fd), syscall.IPPROTO_TCP, syscall.TCP_NODELAY, 1)
				if err != nil {
					log.Errorf("setsocketopt TCP_NODELAY: %s\n", err)
				}
			})
			if err != nil {
				log.Fatal(err)
				continue
			}
		}

		if !singleplex && (sesh == nil || sesh.IsClosed()) {
			sesh = newSeshFunc()
		}
		go func(sesh *mux.Session, localConn net.Conn, timeout time.Duration) {
			if singleplex {
				sesh = newSeshFunc()
			}

			data := make([]byte, 10240)
			_ = localConn.SetReadDeadline(time.Now().Add(streamTimeout))
			i, err := io.ReadAtLeast(localConn, data, 1)
			if err != nil {
				log.Errorf("Failed to read first packet from proxy client: %v", err)
				localConn.Close()
				return
			}
			var zeroTime time.Time
			_ = localConn.SetReadDeadline(zeroTime)

			stream, err := sesh.OpenStream()
			if err != nil {
				log.Errorf("Failed to open stream: %v", err)
				localConn.Close()
				if singleplex {
					sesh.Close()
				}
				return
			}

			_, err = stream.Write(data[:i])
			if err != nil {
				log.Errorf("Failed to write to stream: %v", err)
				localConn.Close()
				stream.Close()
				return
			}

			go func() {
				if _, err := common.Copy(localConn, stream); err != nil {
					log.Tracef("copying stream to proxy client: %v", err)
				}
			}()
			if _, err = common.Copy(stream, localConn); err != nil {
				log.Tracef("copying proxy client to stream: %v", err)
			}
		}(sesh, localConn, streamTimeout)
	}
}
