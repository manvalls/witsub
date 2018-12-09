package witsub

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/gorilla/websocket"
	"github.com/manvalls/wit"
)

// Connection represents an incoming connection
type Connection struct {
	url.Values
	context.Context
	Out chan<- wit.Action
	In  <-chan url.Values

	fakeReq *http.Request
}

// Handler handles incoming websockets
type Handler struct {
	Handler  func(conn Connection)
	Error    func(error)
	InBuffer int
	websocket.Upgrader
}

func (h Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := h.Upgrade(w, r, nil)

	if err != nil {
		if h.Error != nil {
			h.Error(err)
		}
		return
	}

	rootCtx, rootCancel := context.WithCancel(r.Context())
	defer rootCancel()

	mutex := sync.Mutex{}
	mapsLock := sync.Mutex{}
	cancels := make(map[string]context.CancelFunc)
	inChannels := make(map[string]chan url.Values, h.InBuffer)

	cleanup := func(id string) {
		mapsLock.Lock()
		defer mapsLock.Unlock()

		cancel, ok := cancels[id]
		if ok {
			cancel()
			delete(cancels, id)

			mutex.Lock()
			defer mutex.Unlock()

			werr := conn.WriteMessage(websocket.TextMessage, []byte(id))
			if werr != nil && h.Error != nil {
				h.Error(werr)
			}
		}

		chIn, ok := inChannels[id]
		if ok {
			close(chIn)
			delete(inChannels, id)
		}
	}

	defer func() {
		mapsLock.Lock()
		defer mapsLock.Unlock()

		for _, cancel := range cancels {
			cancel()
		}

		for _, chIn := range inChannels {
			close(chIn)
		}

		cancels = nil
		inChannels = nil
	}()

	for {
		_, p, err := conn.ReadMessage()
		if err != nil {
			if h.Error != nil {
				h.Error(err)
			}
			return
		}

		parts := strings.Split(string(p), "\n")
		l := len(parts)

		if l == 1 || l == 3 {
			id := parts[0]
			cleanup(id)

			if l == 3 {
				params, _ := url.ParseQuery(parts[1])

				cookiesHeader := http.Header{}
				cookiesHeader.Add("Cookie", parts[2])
				fakeReq := &http.Request{Header: cookiesHeader}

				ctx, cancel := context.WithCancel(rootCtx)
				chOut := make(chan wit.Action)
				chIn := make(chan url.Values)

				mapsLock.Lock()
				cancels[id] = cancel
				inChannels[id] = chIn
				mapsLock.Unlock()

				go func() {
					defer cleanup(id)

					for {
						select {
						case action, ok := <-chOut:
							if !ok {
								return
							}

							mutex.Lock()

							nw, werr := conn.NextWriter(websocket.TextMessage)
							if werr != nil {
								if h.Error != nil {
									h.Error(werr)
								}

								mutex.Unlock()
								return
							}

							nw.Write([]byte(id))
							nw.Write([]byte{'\n'})
							rerr := wit.NewJSONRenderer(action).Render(nw)
							nw.Close()

							if rerr != nil {
								if h.Error != nil {
									h.Error(rerr)
								}

								mutex.Unlock()
								return
							}

							mutex.Unlock()

						case <-ctx.Done():
							return
						}
					}
				}()

				go func() {
					h.Handler(Connection{params, ctx, chOut, chIn, fakeReq})
					cleanup(id)
				}()
			}
		} else if l == 2 {
			id := parts[0]
			go func() {
				mapsLock.Lock()
				defer mapsLock.Unlock()

				chIn, ok := inChannels[id]
				if ok {
					params, _ := url.ParseQuery(parts[1])
					select {
					case chIn <- params:
					default:
					}
				}
			}()
		}
	}
}
