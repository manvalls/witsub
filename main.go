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

// Request contains the requested parameters
type Request struct {
	url.Values
	context.Context

	fakeReq *http.Request
}

// Handler handles incoming websockets
type Handler struct {
	Handler func(ch chan<- wit.Delta, req Request)
	Error   func(error)
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
	cancels := make(map[string]context.CancelFunc)

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
			cancel, ok := cancels[id]
			if ok {
				delete(cancels, id)
				cancel()
			}

			if l == 3 {
				params, _ := url.ParseQuery(parts[1])

				cookiesHeader := http.Header{}
				cookiesHeader.Add("Cookie", parts[2])
				fakeReq := &http.Request{Header: cookiesHeader}

				ctx, cancel := context.WithCancel(rootCtx)
				cancels[id] = cancel

				ch := make(chan wit.Delta)
				go func() {
					for {
						select {
						case delta, ok := <-ch:
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
							rerr := wit.NewJSONRenderer(delta).Render(nw)
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

				go h.Handler(ch, Request{params, ctx, fakeReq})
			}
		}
	}
}
