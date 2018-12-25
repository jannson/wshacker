package main

import (
	"bytes"
	"container/heap"
	"encoding/binary"
	"log"
	"net"
	"sync"
	"time"
)

var (
	Answer = []byte("\x00\x01\x00\x01\xff\xff\xff\xff\x00\x04\x7f\x00\x00\x01")
)

type dnsReverser interface {
	isReverse(host string) bool
}

type DnsProxy struct {
	dnsHack     string
	udpListen   *net.UDPConn
	udpUpstream *net.UDPConn
	serverAddr  *net.UDPAddr

	entries  []entry
	indices  map[int]int
	mu       sync.Mutex
	chWakeUp chan struct{}

	reverser dnsReverser
}

type entry struct {
	sid int
	ts  time.Time
	q   *DnsQuery
}

type DnsQuery struct {
	sid  int
	addr *net.UDPAddr
	host string
}

func (h *DnsProxy) Len() int           { return len(h.entries) }
func (h *DnsProxy) Less(i, j int) bool { return h.entries[i].ts.Before(h.entries[j].ts) }
func (h *DnsProxy) Swap(i, j int) {
	h.entries[i], h.entries[j] = h.entries[j], h.entries[i]
	h.indices[h.entries[i].sid] = i
	h.indices[h.entries[j].sid] = j
}

func (h *DnsProxy) Push(x interface{}) {
	h.entries = append(h.entries, x.(entry))
	n := len(h.entries)
	h.indices[h.entries[n-1].sid] = n - 1
}

func (h *DnsProxy) Pop() interface{} {
	n := len(h.entries)
	x := h.entries[n-1]
	h.entries[n-1] = entry{}
	h.entries = h.entries[0 : n-1]
	delete(h.indices, x.sid)
	return x
}

func (h *DnsProxy) addQuery(q *DnsQuery) {
	h.mu.Lock()
	heap.Push(h, entry{q.sid, time.Now().Add(time.Second * 10), q})
	h.mu.Unlock()
	h.wakeup()
}

func (h *DnsProxy) removeQuery(sid int) *DnsQuery {
	var q *DnsQuery
	h.mu.Lock()
	if idx, ok := h.indices[sid]; ok {
		el := heap.Remove(h, idx).(entry)
		q = el.q
	}
	h.mu.Unlock()
	return q
}

func (h *DnsProxy) wakeup() {
	select {
	case h.chWakeUp <- struct{}{}:
	default:
	}
}

func (h *DnsProxy) updateTask() {
	var timer <-chan time.Time
	for {
		select {
		case <-timer:
		case <-h.chWakeUp:
		}

		h.mu.Lock()
		hlen := h.Len()
		now := time.Now()
		for i := 0; i < hlen; i++ {
			entry := heap.Pop(h).(entry)
			if now.After(entry.ts) {
				//timeout
				delete(h.indices, entry.sid)
				log.Println("timeout sid=", entry.sid)
			} else {
				heap.Push(h, entry)
				break
			}
		}
		if h.Len() > 0 {
			timer = time.After(h.entries[0].ts.Sub(now))
		}
		h.mu.Unlock()
	}
}

func runDns(listenAddr string, dnsServer string, dnsr dnsReverser) {
	saddr, err := net.ResolveUDPAddr("udp", listenAddr+":53")
	if err != nil {
		log.Fatal(err)
		return
	}
	log.Println("dnsHack", listenAddr, "dnsServer", dnsServer)

	udpListen, err := net.ListenUDP("udp", saddr)
	if err != nil {
		log.Fatal(err)
		return
	}
	defer udpListen.Close()

	dns, err := newDnsProxy(listenAddr, udpListen, dnsServer, dnsr)
	if err != nil {
		log.Fatal(err)
		return
	}
	defer dns.udpUpstream.Close()

	go dns.updateTask()

	go func() {
		for {
			if err := dns.processUpstream(); err != nil {
				break
			}
		}
	}()

	for {
		if err := dns.handleUDP(); err != nil {
			return
		}
	}
}

func newDnsProxy(dnsHack string, udpListen *net.UDPConn, dnsServer string, dnsr dnsReverser) (*DnsProxy, error) {
	udpUpstream, err := net.ListenUDP("udp", &net.UDPAddr{})
	if err != nil {
		return nil, err
	}
	serverAddr, err := net.ResolveUDPAddr("udp", dnsServer)
	if err != nil {
		return nil, err
	}

	return &DnsProxy{
		dnsHack:     dnsHack,
		udpListen:   udpListen,
		udpUpstream: udpUpstream,
		serverAddr:  serverAddr,

		entries:  make([]entry, 0),
		indices:  make(map[int]int),
		chWakeUp: make(chan struct{}, 1),
		reverser: dnsr,
	}, nil
}

func (dns *DnsProxy) handleUDP() error {
	buf := make([]byte, 4096)
	n, addr, err := dns.udpListen.ReadFromUDP(buf)
	if err != nil {
		return err
	}
	buf = buf[:n]

	queryId := int(binary.BigEndian.Uint16(buf[0:2]))
	host := dns.hostFromMsg(buf)

	isReverse := dns.reverser.isReverse(host)
	if isReverse {
		log.Println("dns query:", host, "hack it to:", dns.dnsHack)
		dns.hackDns(buf, host, dns.dnsHack, addr)
	} else {
		log.Println("dns query:", host)
		q := &DnsQuery{
			sid:  queryId,
			addr: addr,
			host: host,
		}
		dns.addQuery(q)

		if _, err := dns.udpUpstream.WriteToUDP(buf, dns.serverAddr); err != nil {
			return err
		}
	}

	return nil
}

func (dns *DnsProxy) hostFromMsg(msg []byte) string {
	var domain bytes.Buffer
	var host string

	count := uint8(msg[5]) // question counter
	offset := 12           // point to first domain name
	if count == 1 {
		for {
			length := int8(msg[offset])
			if length == 0 {
				break
			}
			offset++
			domain.WriteString(string(msg[offset : offset+int(length)]))
			domain.WriteString(".")
			offset += int(length)
		}

		host = domain.String()
		return host
	}
	return ""
}

func (dns *DnsProxy) hackDns(msg []byte, host string, target string, addr *net.UDPAddr) {
	targetIp := net.ParseIP(target)
	msg[2] = uint8(129)                           // flags upper byte
	msg[3] = uint8(128)                           // flags lower byte
	msg[7] = uint8(1)                             // answer counter
	msg = append(msg, msg[12:12+1+len(host)]...)  // domain
	msg = append(msg, Answer[0:len(Answer)-4]...) // payload
	msg = append(msg, []byte(targetIp.To4())...)

	dns.udpListen.WriteTo(msg, addr)
}

func (dns *DnsProxy) processUpstream() error {
	buf := make([]byte, 4096)
	n, _, err := dns.udpUpstream.ReadFromUDP(buf)
	if err == nil {
		buf = buf[:n]
		queryId := int(binary.BigEndian.Uint16(buf[0:2]))
		q := dns.removeQuery(queryId)
		if q != nil {
			dns.udpListen.WriteTo(buf, q.addr)
		}
		return nil
	} else {
		return err
	}
}
