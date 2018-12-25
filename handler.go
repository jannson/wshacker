package main

import (
	"log"
	"net"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
)

type wsServer struct {
	upgrader *websocket.Upgrader
}

type wsHandler struct {
	isTls bool
	s     *wsServer
}

func (h *wsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	log.Println("r.URL", r.URL, "RemoteAddr", r.RemoteAddr, "tls", h.isTls)

	if r.Header.Get("Upgrade") == "websocket" {
		err := h.s.serveWebSocket(w, r)
		if err != nil {
			log.Println("serve websocket failed", err)
		}
		return
	}

	http.Error(w, "not ws connection", 500)
}

func (s *wsServer) serveWebSocket(w http.ResponseWriter, r *http.Request) error {
	c, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return err
	}
	defer c.Close()
	c.SetReadDeadline(time.Time{})
	c.SetWriteDeadline(time.Time{})

	key := r.Header.Get("Sec-WebSocket-Key")
	log.Println("new websocket key=", key)

	var scheme string
	if r.URL.Scheme == "https" {
		scheme = "wss://"
	} else {
		scheme = "ws://"
	}
	wsURL := scheme + r.Host + r.RequestURI
	hd := http.Header{}
	hd.Set("Host", r.Host)
	hd.Set("Origin", r.URL.Scheme+"://"+r.Host)
	wsc, resp, err := websocket.DefaultDialer.Dial(wsURL, hd)
	if err != nil {
		log.Println("wsURL", wsURL, err, "key=", key)
		return err
	}
	defer resp.Body.Close()
	defer wsc.Close()
	wsc.SetReadDeadline(time.Time{})
	wsc.SetWriteDeadline(time.Time{})

	//
	pf := func(a, b *websocket.Conn, tag string, ch chan struct{}, msg int) func(s string) error {
		var text string
		if msg == websocket.PingMessage {
			text = "ping"
		} else {
			text = "pong"
		}
		return func(message string) error {
			log.Println(tag, "side read  key=", key, "msg=", text)
			err := b.WriteControl(msg, []byte(message), time.Now().Add(time.Second))
			if err == websocket.ErrCloseSent {
				return nil
			} else if e, ok := err.(net.Error); ok && e.Temporary() {
				return nil
			} else if err != nil {
				close(ch)
				log.Println(tag, "side writer closed key=", key, "msg=", text)
			}
			return err
		}
	}
	wsf := func(a, b *websocket.Conn, tag string, ch chan struct{}) {
		a.SetPingHandler(pf(a, b, tag, ch, websocket.PingMessage))
		a.SetPongHandler(pf(a, b, tag, ch, websocket.PongMessage))
		for {
			mt, message, err := a.ReadMessage()
			if err != nil {
				close(ch)
				log.Println(tag, "side reader closed key=", key)
				break
			}
			if mt == websocket.TextMessage {
				log.Println(tag, "side get text message:", mt, "key=", key)
				log.Println(string(message))
			} else {
				log.Println(tag, "side get binary message:", mt, "key=", key)
				log.Println(message)
			}
			err = b.WriteMessage(mt, message)
			if err != nil {
				close(ch)
				log.Println(tag, "side writer closed key=", key)
				break
			}
		}
	}

	p1die := make(chan struct{})
	go func() {
		wsf(c, wsc, "client", p1die)
	}()

	p2die := make(chan struct{})
	go func() {
		wsf(wsc, c, "server", p2die)
	}()

	select {
	case <-p1die:
	case <-p2die:
	}

	return nil
}
