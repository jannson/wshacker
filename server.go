package main

import (
	"crypto/tls"
	"flag"
	"io/ioutil"
	"log"
	"net"
	"net/http"

	"github.com/gorilla/websocket"
)

func mustReadFile(p string) []byte {
	b, err := ioutil.ReadFile(p)
	if err != nil {
		panic(err)
	}
	return b
}

func getOutboundIP(s string) (net.IP, error) {
	conn, err := net.Dial("udp", s)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	localAddr := conn.LocalAddr().(*net.UDPAddr)

	return localAddr.IP, nil
}

func main() {
	var full, priv, httpAddr, https, hosts, dnsServer string
	flag.StringVar(&full, "full", "fullchain.pem", "fullchain pem")
	flag.StringVar(&priv, "priv", "privkey.pem", "private key")
	flag.StringVar(&httpAddr, "http", ":80", "http addr")
	flag.StringVar(&https, "https", ":443", "https addr")
	flag.StringVar(&dnsServer, "dns", "10.1.150.1:53", "dns server addr")
	flag.StringVar(&hosts, "hosts", "hosts", "hosts file to reverse dns")
	flag.Parse()

	tlsConfig := &tls.Config{}
	tlsConfig.Certificates = make([]tls.Certificate, 0)

	certifi, err := tls.X509KeyPair(mustReadFile(full),
		mustReadFile(priv))
	if err != nil {
		log.Fatal(err)
	}
	tlsConfig.Certificates = append(tlsConfig.Certificates, certifi)
	tlsConfig.BuildNameToCertificate()

	s := &wsServer{
		reverseUrlMap: make(map[string]int),
		rt:            &http.Transport{},
		upgrader: &websocket.Upgrader{
			ReadBufferSize:    16384,
			WriteBufferSize:   16384,
			EnableCompression: true,
			Subprotocols:      []string{"zuolin"},
			CheckOrigin: func(r *http.Request) bool {
				return true
			},
			Error: func(w http.ResponseWriter, r *http.Request, status int, reason error) {
				http.Error(w, reason.Error(), status)
			},
		},
	}

	err = s.parseDNS(hosts)
	if err != nil {
		panic(err)
	}

	localIP, err := getOutboundIP(dnsServer)
	if err != nil {
		panic(err)
	}
	log.Println("got localIP", localIP)
	go runDns(localIP.String(), dnsServer, s)

	lhttp, err := net.Listen("tcp", httpAddr)
	if err != nil {
		log.Fatal(err)
	}
	log.Println("run http ok", httpAddr)
	httpServer := &http.Server{
		Handler: &wsHandler{false, s},
	}
	go httpServer.Serve(lhttp)

	lhttps, err := tls.Listen("tcp", https, tlsConfig)
	if err != nil {
		log.Fatal(err)
	}
	log.Println("run https ok", https)
	httpsServer := &http.Server{
		Handler: &wsHandler{true, s},
	}
	httpsServer.Serve(lhttps)
}
