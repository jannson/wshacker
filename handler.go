package main

import (
	"bufio"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

type wsServer struct {
	reversePrefix map[string]int
	reverseUrlMap map[string]int
	rt            *http.Transport
	upgrader      *websocket.Upgrader
}

type wsHandler struct {
	isTls bool
	s     *wsServer
}

var hopHeaders = map[string]int{
	"Connection":          1,
	"Proxy-Connection":    1, // non-standard but still sent by libcurl and rejected by e.g. google
	"Keep-Alive":          1,
	"Proxy-Authenticate":  1,
	"Proxy-Authorization": 1,
	"Te":                  1, // canonicalized version of "TE"
	"Trailer":             1, // not Trailers per URL above; http://www.rfc-editor.org/errata_search.php?eid=4522
	"Transfer-Encoding":   1,
	"Upgrade":             1,
	"Content-Length":      1,
}

func copyResponseHeader(w http.ResponseWriter, resp *http.Response) {
	newHeader := w.Header()
	for key, values := range resp.Header {
		if _, ok := hopHeaders[key]; !ok {
			for _, v := range values {
				newHeader.Add(key, v)
			}
		}
	}
	//log.Println("resp header", resp.Header, newHeader)
}

func (h *wsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r.URL.Host = r.Host
	if h.isTls {
		r.URL.Scheme = "https"
	} else {
		r.URL.Scheme = "http"
	}
	log.Println("URL", r.URL, "RemoteAddr", r.RemoteAddr)

	if r.Header.Get("Upgrade") == "websocket" {
		err := h.s.serveWebSocket(w, r)
		if err != nil {
			log.Println("serve websocket failed", err)
		}
		return
	}

	resp, err := h.s.rt.RoundTrip(r)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	defer resp.Body.Close()
	copyResponseHeader(w, resp)
	if resp.ContentLength > 0 {
		w.Header().Set("Content-Length", strconv.Itoa(int(resp.ContentLength)))
	} else {
		w.Header().Set("Transfer-Encoding", "chunked")
	}
	w.WriteHeader(resp.StatusCode)

	if n, err := io.Copy(w, resp.Body); err != nil {
		log.Printf("error while copying response (URL: %s): %s copy-length: %d", r.URL, err, n)
	}
	return
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

func (s *wsServer) parseDNS(hosts string) error {
	file, err := os.Open(hosts)
	if err != nil {
		return err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		dns := scanner.Text()
		if len(dns) > 2 {
			if dns[0:2] == "*." {
				s.reversePrefix[dns[2:len(dns)]] = 1
			} else {
				s.reverseUrlMap[dns] = 1
			}
		}
	}
	return nil
}

func (s *wsServer) isReverse(host string) bool {
	if len(host) > 0 && host[len(host)-1] == '.' {
		host = host[0 : len(host)-1]
	}
	if _, ok := s.reverseUrlMap[host]; ok {
		return true
	}

	ss := strings.Split(host, ".")
	for len(ss) > 1 {
		newhost := strings.Join(ss, ".")
		if _, ok := s.reversePrefix[newhost]; ok {
			return true
		}
		ss = ss[1:len(ss)]
	}

	return false
}
