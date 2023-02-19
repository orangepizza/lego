// Package nfqueue implements a HTTP provider for solving the HTTP-01 challenge using nfqueue
// by captureing http challange pacet in fly and answering it by ourself
package nfqueue

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"strings"
	"time"

	gnfqueue "github.com/florianl/go-nfqueue"
	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
)

// HTTPProvider implements HTTPProvider for `http-01` challenge.
type HTTPProvider struct {
	port    string
	context context.Context
	cancel  context.CancelFunc
}

// NewHttpDpiProvider returns a HTTPProvider instance with a configured port.
func NewHttpDpiProvider(port string) (*HTTPProvider, error) {

	c := &HTTPProvider{
		port: port,
	}

	return c, nil
}

// this craft acme challange response in HTTP level
func craftkeyauthresponse(keyAuth string) []byte {
	var reply []byte
	reply = fmt.Append(reply, "HTTP/1.1 200 OK\r\n")
	reply = fmt.Append(reply, "Content-Type: text/plain\r\n")
	reply = fmt.Append(reply, "server: go-acme-nfqueue\r\n")
	reply = fmt.Appendf(reply, "Content-Length: %d\r\n", len(keyAuth))
	reply = fmt.Append(reply, "\r\n", keyAuth)

	return reply
}

// craft packet
func craftReplyandSend(keyAuth string, inputpacket gopacket.Packet) error {
	outbuffer := gopacket.NewSerializeBuffer()
	opt := gopacket.SerializeOptions{
		FixLengths:       true,
		ComputeChecksums: true,
	}
	inputTcp := inputpacket.Layer(layers.LayerTypeTCP).(*layers.TCP)
	inputIPv4 := inputpacket.Layer(layers.LayerTypeIPv4).(*layers.IPv4)

	httplayer := gopacket.Payload(craftkeyauthresponse(keyAuth))
	tcplayer := &layers.TCP{
		// we reply back so reverse src and dst ports
		SrcPort: inputTcp.DstPort,
		DstPort: inputTcp.SrcPort,
		Ack:     inputTcp.Seq + uint32(len(inputTcp.Payload)),
		Seq:     inputTcp.Ack,
		Window:  1,
		PSH:     true,
		ACK:     true,
	}
	// log.Infof("dstp: %s, srcp %s", tcplayer.DstPort.String(), tcp)
	// check network layer
	// this is reply so we reverse sorce and dst ip
	iplayer := &layers.IPv4{
		SrcIP: inputIPv4.DstIP,
		DstIP: inputIPv4.SrcIP,
	}
	tcplayer.SetNetworkLayerForChecksum(iplayer)
	gopacket.SerializeLayers(outbuffer, opt, tcplayer, httplayer)
	// send http reply
	sendPacket(outbuffer.Bytes(), &iplayer.DstIP)

	// craft RST packet to server so connection can close by webserver
	outbuffer.Clear()
	tcplayer.RST = true
	tcplayer.ACK = false
	tcplayer.PSH = false
	tcplayer.Seq = tcplayer.Seq + uint32(len(httplayer.Payload()))

	tcplayer.SetNetworkLayerForChecksum(iplayer)
	gopacket.SerializeLayers(outbuffer, opt, tcplayer)
	// rst to acme server so it knows it's done,
	// our webserver will send some ack packet but it has misalign Seq so ignored by ACME server
	sendPacket(outbuffer.Bytes(), &iplayer.DstIP)

	return nil
}

// sendPacket sends packet: TODO: call cleanup if errors out
func sendPacket(packet []byte, DstIP *net.IP) error {
	var err error
	con, err := net.Dial("ip:6", DstIP.String())
	if err != nil {
		return err
	}
	_, err = con.Write(packet)
	if err != nil {
		return err
	}
	return nil
}

// serve runs server by sniffing packets on firewall and inject response into it.
// iptables ://
func (w *HTTPProvider) serve(domain, token, keyAuth string) error {
	// run nfqueue start
	cmd := exec.Command("iptables", "-I", "INPUT", "-p", "tcp", "--dport", w.port, "-j", "NFQUEUE", "--queue-num", "8555")
	err := cmd.Run()
	// ensure even if clean funtion failed to called
	defer exec.Command("iptables", "-D", "INPUT", "-p", "tcp", "--dport", w.port, "-j", "NFQUEUE", "--queue-num", "8555").Run()
	if err != nil {
		return err
	}
	config := gnfqueue.Config{
		NfQueue:      8555,
		MaxPacketLen: 0xFFFF,
		MaxQueueLen:  0xFF,
		Copymode:     gnfqueue.NfQnlCopyPacket,
		WriteTimeout: 15 * time.Millisecond,
	}
	nf, err := gnfqueue.Open(&config)
	if err != nil {
		return err
	}
	defer nf.Close()

	// handle Packet
	handlepacket := func(a gnfqueue.Attribute) int {
		id := *a.PacketID
		opt := gopacket.DecodeOptions{
			NoCopy: true,
			Lazy:   false,
		}
		// assume ipv4 for now, will segfault
		payload := gopacket.NewPacket(*a.Payload, layers.LayerTypeIPv4, opt)
		if tcpLayer := payload.Layer(layers.LayerTypeTCP); tcpLayer != nil {
			// Get actual TCP data from this layer
			inputTcp, _ := tcpLayer.(*layers.TCP)
			// this should be HTTP payload
			httpPayload, err := http.ReadRequest(bufio.NewReader((bytes.NewReader(inputTcp.LayerPayload()))))
			if err != nil {
				nf.SetVerdict(id, gnfqueue.NfAccept)
				return 0
			}
			// check token in http
			if strings.Contains(httpPayload.URL.Path, token) {
				// we got the token!, block the packet to backend server.
				nf.SetVerdict(id, gnfqueue.NfDrop)
				// forge our new reply
				err := craftReplyandSend(keyAuth, payload)
				if err != nil {
					return 0
				}
				// packet sent, end of function
				return 0
			} else {
				nf.SetVerdict(id, gnfqueue.NfAccept)
				return 0
			}

		} else {
			nf.SetVerdict(id, gnfqueue.NfAccept)
		}

		return 0
	}

	ignoreerr := func(err error) int {
		log.Print(err)
		return 0
	}

	// Register your function to listen on nflqueue queue
	err = nf.RegisterWithErrorFunc(w.context, handlepacket, ignoreerr)
	if err != nil {
		fmt.Println(err)
		return nil
	}

	// Block till the context expires
	<-w.context.Done()
	return nil
}

func (w *HTTPProvider) Present(domain, token, keyAuth string) error {
	// test if OS is linux, otherwise no point running this nfqueue is linux thing
	if runtime.GOOS != "linux" {
		log.Panicf("[%s] http-nfq provider isn't implimented non-linux", domain)
	}
	w.context, w.cancel = context.WithCancel(context.Background())
	go w.serve(domain, token, keyAuth)
	return nil
}

// CleanUp removes the firewall rule created for the challenge.
// solve should removed it already but just do be safe:
// iptables -D INPUT -p tcp --dport Port -j NFQUEUE --queue-num 8555
func (w *HTTPProvider) CleanUp(domain, token, keyAuth string) error {
	cmd := exec.Command("iptables", "-D", "INPUT", "-p", "tcp", "--dport", w.port, "-j", "NFQUEUE", "--queue-num", "8555")
	cmd.Run()
	// tell nfqueue to shut down
	w.cancel()
	return nil
}
