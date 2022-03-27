package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/url"
	"strings"
	"sync"
	"time"

	"encoding/json"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v3"
	websocketapi "github.com/pojntfx/webrtcfd/internal/api/websocket"
	"github.com/pojntfx/webrtcfd/internal/encryption"
)

var (
	errMissingCommunity = errors.New("missing community")
	errMissingPassword  = errors.New("missing password")

	errMissingKey = errors.New("missing key")

	errInvalidTURNServerAddr  = errors.New("invalid TURN server address")
	errMissingTURNCredentials = errors.New("missing TURN server credentials")
)

const (
	dataChannelName = "webrtcfd"
)

type peer struct {
	conn       *webrtc.PeerConnection
	candidates chan webrtc.ICECandidateInit
	channel    *webrtc.DataChannel
}

func main() {
	raddr := flag.String("raddr", "wss://webrtcfd.herokuapp.com/", "Remote address")
	timeout := flag.Duration("timeout", time.Second*10, "Time to wait for connections")
	community := flag.String("community", "", "ID of community to join")
	password := flag.String("password", "", "Password for community")
	key := flag.String("key", "", "Encryption key for community")
	stun := flag.String("stun", "stun:stun.l.google.com:19302", "Comma-seperated list of STUN servers to use (in format stun:host:port)")
	turn := flag.String("turn", "", "Comma-seperated list of TURN servers to use (in format username:credential@turn:host:port) (i.e. username:credential@turn:global.turn.twilio.com:3478?transport=tcp)")
	verbose := flag.Bool("verbose", false, "Enable verbose logging")

	flag.Parse()

	if strings.TrimSpace(*community) == "" {
		panic(errMissingCommunity)
	}

	if strings.TrimSpace(*password) == "" {
		panic(errMissingPassword)
	}

	if strings.TrimSpace(*key) == "" {
		panic(errMissingKey)
	}

	log.Println("Connecting to signaler with address", *raddr)

	u, err := url.Parse(*raddr)
	if err != nil {
		panic(err)
	}

	q := u.Query()
	q.Set("community", *community)
	q.Set("password", *password)
	u.RawQuery = q.Encode()

	iceServers := []webrtc.ICEServer{}

	for _, stunServer := range strings.Split(*stun, ",") {
		// Skip empty STUN server configs
		if strings.TrimSpace(stunServer) == "" {
			continue
		}

		iceServers = append(iceServers, webrtc.ICEServer{
			URLs: []string{stunServer},
		})
	}

	for _, turnServer := range strings.Split(*turn, ",") {
		// Skip empty TURN server configs
		if strings.TrimSpace(turnServer) == "" {
			continue
		}

		addrParts := strings.Split(turnServer, "@")
		if len(addrParts) < 2 {
			panic(errInvalidTURNServerAddr)
		}

		authParts := strings.Split(addrParts[0], ":")
		if len(addrParts) < 2 {
			panic(errMissingTURNCredentials)
		}

		iceServers = append(iceServers, webrtc.ICEServer{
			URLs:           []string{addrParts[1]},
			Username:       authParts[0],
			Credential:     authParts[1],
			CredentialType: webrtc.ICECredentialTypePassword,
		})
	}

	lines := make(chan []byte)
	defer close(lines)

	peers := map[string]*peer{}
	var peerLock sync.Mutex

	for {
		func() {
			defer func() {
				if err := recover(); err != nil {
					log.Println("closed connection to signaler with address", *raddr+":", err, "(wrong username or password?)")
				}

				log.Println("Reconnecting to signaler with address", *raddr, "in", *timeout)

				time.Sleep(*timeout)
			}()

			ctx, cancel := context.WithTimeout(context.Background(), *timeout)
			defer cancel()

			conn, _, err := websocket.DefaultDialer.DialContext(ctx, u.String(), nil)
			if err != nil {
				panic(err)
			}

			defer func() {
				log.Println("Disconnected from signaler with address", *raddr)

				if err := conn.Close(); err != nil {
					panic(err)
				}

				for _, peer := range peers {
					if err := peer.conn.Close(); err != nil {
						panic(err)
					}

					close(peer.candidates)
				}
			}()

			log.Println("Connected to signaler with address", *raddr)

			inputs := make(chan []byte)
			errs := make(chan error)
			go func() {
				defer func() {
					close(inputs)
					close(errs)
				}()

				for {
					_, p, err := conn.ReadMessage()
					if err != nil {
						errs <- err

						return
					}

					inputs <- p
				}
			}()

			id := uuid.New().String()

			go func() {
				p, err := json.Marshal(websocketapi.NewIntroduction(id))
				if err != nil {
					errs <- err

					return
				}

				lines <- p

				log.Println("Introduced to signaler with address", *raddr, "and ID", id)
			}()

			for {
				select {
				case err := <-errs:
					panic(err)
				case input := <-inputs:
					input, err = encryption.Decrypt(input, []byte(*key))
					if err != nil {
						if *verbose {
							log.Println("Could not decrypt message with length", len(input), "for signaler with address", conn.RemoteAddr(), "in community", *community+", skipping")
						}

						continue
					}

					if *verbose {
						log.Println("Received message with length", len(input), "from signaler with address", conn.RemoteAddr(), "in community", *community)
					}

					var message websocketapi.Message
					if err := json.Unmarshal(input, &message); err != nil {
						if *verbose {
							log.Println("Could not unmarshal message for signaler with address", conn.RemoteAddr(), "in community", *community+", skipping")
						}

						continue
					}

					switch message.Type {
					case websocketapi.TypeIntroduction:
						var introduction websocketapi.Introduction
						if err := json.Unmarshal(input, &introduction); err != nil {
							if *verbose {
								log.Println("Could not unmarshal introduction for signaler with address", conn.RemoteAddr(), "in community", *community+", skipping")
							}

							continue
						}

						if *verbose {
							log.Println("Received introduction", introduction, "from signaler with address", conn.RemoteAddr(), "in community", *community)
						}

						c, err := webrtc.NewPeerConnection(webrtc.Configuration{
							ICEServers: iceServers,
						})
						if err != nil {
							panic(err)
						}

						c.OnConnectionStateChange(func(pcs webrtc.PeerConnectionState) {
							if pcs == webrtc.PeerConnectionStateDisconnected {
								log.Println("Disconnected from peer", introduction.From)

								peerLock.Lock()
								defer peerLock.Unlock()

								c, ok := peers[introduction.From]

								if !ok {
									if *verbose {
										log.Println("Could not find connection for peer", introduction.From, ", skipping")
									}

									return
								}

								if err := c.channel.Close(); err != nil {
									panic(err)
								}

								if err := c.conn.Close(); err != nil {
									panic(err)
								}

								close(c.candidates)

								delete(peers, introduction.From)
							}
						})

						c.OnICECandidate(func(i *webrtc.ICECandidate) {
							if i != nil {
								if *verbose {
									log.Println("Created ICE candidate", i, "for signaler with address", conn.RemoteAddr(), "in community", *community)
								}

								p, err := json.Marshal(websocketapi.NewCandidate(id, introduction.From, []byte(i.ToJSON().Candidate)))
								if err != nil {
									panic(err)
								}

								go func() {
									lines <- p

									if *verbose {
										log.Println("Sent candidate to signaler with address", *raddr, "and ID", id, "to client", introduction.From)
									}
								}()
							}
						})

						dc, err := c.CreateDataChannel(dataChannelName, nil)
						if err != nil {
							panic(err)
						}
						closed := false

						if *verbose {
							log.Println("Created data channel using signaler with address", conn.RemoteAddr(), "in community", *community)
						}

						dc.OnOpen(func() {
							log.Println("Connected to peer", introduction.From)

							for {
								time.Sleep(time.Second)

								if closed {
									return
								}

								if err := dc.Send([]byte("Hello from peer " + id)); err != nil {
									panic(err)
								}

								if *verbose {
									log.Println("Sent message to peer", introduction.From)
								}
							}
						})

						dc.OnClose(func() {
							closed = true
						})

						dc.OnMessage(func(msg webrtc.DataChannelMessage) {
							log.Println("Received message from", introduction.From, string(msg.Data))
						})

						o, err := c.CreateOffer(nil)
						if err != nil {
							panic(err)
						}

						if err := c.SetLocalDescription(o); err != nil {
							panic(err)
						}

						oj, err := json.Marshal(o)
						if err != nil {
							panic(err)
						}

						p, err := json.Marshal(websocketapi.NewOffer(id, introduction.From, oj))
						if err != nil {
							panic(err)
						}

						peerLock.Lock()
						peers[introduction.From] = &peer{c, make(chan webrtc.ICECandidateInit), dc}
						peerLock.Unlock()

						go func() {
							lines <- p

							if *verbose {
								log.Println("Sent offer to signaler with address", *raddr, "and ID", id, "to client", introduction.From)
							}
						}()
					case websocketapi.TypeOffer:
						var offer websocketapi.Exchange
						if err := json.Unmarshal(input, &offer); err != nil {
							if *verbose {
								log.Println("Could not unmarshal offer for signaler with address", conn.RemoteAddr(), "in community", *community+", skipping")
							}

							continue
						}

						if *verbose {
							log.Println("Received offer", offer, "from signaler with address", conn.RemoteAddr(), "in community", *community)
						}

						if offer.To != id {
							if *verbose {
								log.Println("Discarding offer", offer, "from signaler with address", conn.RemoteAddr(), "in community", *community, "because it is not intended for this client")
							}

							continue
						}

						c, err := webrtc.NewPeerConnection(webrtc.Configuration{
							ICEServers: iceServers,
						})
						if err != nil {
							panic(err)
						}

						c.OnConnectionStateChange(func(pcs webrtc.PeerConnectionState) {
							if pcs == webrtc.PeerConnectionStateDisconnected {
								log.Println("Disconnected from peer", offer.From)

								peerLock.Lock()
								defer peerLock.Unlock()

								c, ok := peers[offer.From]

								if !ok {
									if *verbose {
										log.Println("Could not find connection for peer", offer.From, ", skipping")
									}

									return
								}

								if err := c.channel.Close(); err != nil {
									panic(err)
								}

								if err := c.conn.Close(); err != nil {
									panic(err)
								}

								close(c.candidates)

								delete(peers, offer.From)
							}
						})

						c.OnICECandidate(func(i *webrtc.ICECandidate) {
							if i != nil {
								if *verbose {
									log.Println("Created ICE candidate", i, "for signaler with address", conn.RemoteAddr(), "in community", *community)
								}

								p, err := json.Marshal(websocketapi.NewCandidate(id, offer.From, []byte(i.ToJSON().Candidate)))
								if err != nil {
									panic(err)
								}

								go func() {
									lines <- p

									if *verbose {
										log.Println("Sent candidate to signaler with address", *raddr, "and ID", id, "to client", offer.From)
									}
								}()
							}
						})

						c.OnDataChannel(func(dc *webrtc.DataChannel) {
							closed := false

							dc.OnOpen(func() {
								log.Println("Connected to peer", offer.From)

								peerLock.Lock()
								peers[offer.From].channel = dc
								peerLock.Unlock()

								for {
									time.Sleep(time.Second)

									if closed {
										return
									}

									if err := dc.Send([]byte("Hello from peer " + id)); err != nil {
										panic(err)
									}

									if *verbose {
										log.Println("Sent message to peer", offer.From)
									}
								}
							})

							dc.OnClose(func() {
								closed = true
							})

							dc.OnMessage(func(msg webrtc.DataChannelMessage) {
								log.Println("Received message from", offer.From, string(msg.Data))
							})
						})

						var sdp webrtc.SessionDescription
						if err := json.Unmarshal(offer.Payload, &sdp); err != nil {
							if *verbose {
								log.Println("Could not unmarshal SDP for signaler with address", conn.RemoteAddr(), "in community", *community+", skipping")
							}

							continue
						}

						if err := c.SetRemoteDescription(sdp); err != nil {
							panic(err)
						}

						a, err := c.CreateAnswer(nil)
						if err != nil {
							panic(err)
						}

						if err := c.SetLocalDescription(a); err != nil {
							panic(err)
						}

						aj, err := json.Marshal(a)
						if err != nil {
							panic(err)
						}

						p, err := json.Marshal(websocketapi.NewAnswer(id, offer.From, aj))
						if err != nil {
							panic(err)
						}

						peerLock.Lock()

						candidates := make(chan webrtc.ICECandidateInit)
						peers[offer.From] = &peer{c, candidates, nil}

						peerLock.Unlock()

						go func() {
							for candidate := range candidates {
								if err := c.AddICECandidate(candidate); err != nil {
									errs <- err

									return
								}

								if *verbose {
									log.Println("Added ICE candidate from signaler with address", *raddr, "and ID", id, "from client", offer.From)
								}
							}
						}()

						go func() {
							lines <- p

							if *verbose {
								log.Println("Sent answer to signaler with address", *raddr, "and ID", id, "to client", offer.From)
							}
						}()
					case websocketapi.TypeCandidate:
						var candidate websocketapi.Exchange
						if err := json.Unmarshal(input, &candidate); err != nil {
							if *verbose {
								log.Println("Could not unmarshal candidate for signaler with address", conn.RemoteAddr(), "in community", *community+", skipping")
							}

							continue
						}

						if *verbose {
							log.Println("Received candidate", candidate, "from signaler with address", conn.RemoteAddr(), "in community", *community)
						}

						if candidate.To != id {
							if *verbose {
								log.Println("Discarding candidate", candidate, "from signaler with address", conn.RemoteAddr(), "in community", *community, "because it is not intended for this client")
							}

							continue
						}

						peerLock.Lock()
						c, ok := peers[candidate.From]

						if !ok {
							if *verbose {
								log.Println("Could not find connection for peer", candidate.From, ", skipping")
							}

							peerLock.Unlock()

							continue
						}

						go func() {
							defer func() {
								if err := recover(); err != nil {
									if *verbose {
										log.Println("Gathering candidates has stopped, skipping candidate")
									}
								}
							}()

							c.candidates <- webrtc.ICECandidateInit{Candidate: string(candidate.Payload)}
						}()

						peerLock.Unlock()
					case websocketapi.TypeAnswer:
						var answer websocketapi.Exchange
						if err := json.Unmarshal(input, &answer); err != nil {
							if *verbose {
								log.Println("Could not unmarshal answer for signaler with address", conn.RemoteAddr(), "in community", *community+", skipping")
							}

							continue
						}

						if *verbose {
							log.Println("Received answer", answer, "from signaler with address", conn.RemoteAddr(), "in community", *community)
						}

						if answer.To != id {
							if *verbose {
								log.Println("Discarding answer", answer, "from signaler with address", conn.RemoteAddr(), "in community", *community, "because it is not intended for this client")
							}

							continue
						}

						peerLock.Lock()
						c, ok := peers[answer.From]
						peerLock.Unlock()

						if !ok {
							if *verbose {
								log.Println("Could not find connection for peer", answer.From, ", skipping")
							}

							continue
						}

						var sdp webrtc.SessionDescription
						if err := json.Unmarshal(answer.Payload, &sdp); err != nil {
							if *verbose {
								log.Println("Could not unmarshal SDP for signaler with address", conn.RemoteAddr(), "in community", *community+", skipping")
							}

							continue
						}

						if err := c.conn.SetRemoteDescription(sdp); err != nil {
							panic(err)
						}

						go func() {
							for candidate := range c.candidates {
								if err := c.conn.AddICECandidate(candidate); err != nil {
									errs <- err

									return
								}

								if *verbose {
									log.Println("Added ICE candidate from signaler with address", *raddr, "and ID", id, "from client", answer.From)
								}
							}
						}()

						if *verbose {
							log.Println("Added answer from signaler with address", *raddr, "and ID", id, "from client", answer.From)
						}
					default:
						if *verbose {
							log.Println("Got message with unknown type", message.Type, "for signaler with address", conn.RemoteAddr(), "in community", *community+", skipping")
						}

						continue
					}

				case line := <-lines:
					line, err = encryption.Encrypt(line, []byte(*key))
					if err != nil {
						panic(err)
					}

					if *verbose {
						log.Println("Sending message with length", len(line), "to signaler with address", conn.RemoteAddr(), "in community", *community)
					}

					if err := conn.WriteMessage(websocket.TextMessage, line); err != nil {
						panic(err)
					}
				}
			}
		}()
	}
}
