//
// go-sonos
// ========
//
// Copyright (c) 2012, Ian T. Richards <ianr@panix.com>
// All rights reserved.
//
// Redistribution and use in source and binary forms, with or without
// modification, are permitted provided that the following conditions
// are met:
//
//   * Redistributions of source code must retain the above copyright notice,
//     this list of conditions and the following disclaimer.
//   * Redistributions in binary form must reproduce the above copyright
//     notice, this list of conditions and the following disclaimer in the
//     documentation and/or other materials provided with the distribution.
//
// THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS
// "AS IS" AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT
// LIMITED TO, THE IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR
// A PARTICULAR PURPOSE ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT
// HOLDER OR CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL,
// SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT LIMITED
// TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE, DATA, OR
// PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY THEORY OF
// LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT (INCLUDING
// NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE OF THIS
// SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.
//

package upnp

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type upnpEventProperty_XML struct {
	Content string `xml:",innerxml"`
}

type upnpEvent_XML struct {
	XMLName    xml.Name                `xml:"urn:schemas-upnp-org:event-1-0 propertyset"`
	Properties []upnpEventProperty_XML `xml:"urn:schemas-upnp-org:event-1-0 property"`
}

type EventFactory interface {
	BeginSet(svc *Service, channel chan Event)
	HandleProperty(svc *Service, value string, channel chan Event) error
	EndSet(svc *Service, channel chan Event)
}

type Reactor interface {
	Init(ifiname, port string)
	Subscribe(svc *Service, factory EventFactory) error
	Channel() chan Event
}

var (
	nextEventType = 0
	eventTypeMap  = make(map[string]int)
)

func registerEventType(tag string) int {
	if id, has := eventTypeMap[tag]; has {
		return id
	} else {
		eventTypeMap[tag] = nextEventType
		defer func() {
			nextEventType++
		}()
	}
	return nextEventType
}

type upnpEventType int

const (
	upnpEventTypeBeginSet upnpEventType = iota
	upnpEventTypeProperty
	upnpEventTypeEndSet
)

type upnpEvent struct {
	sid   string
	value string
	etype upnpEventType
}

type upnpEventRecord struct {
	svc     *Service
	factory EventFactory
}

type upnpEventMap map[string]*upnpEventRecord

type Event interface {
	Service() *Service
	Type() int
}

type upnpDefaultReactor struct {
	ifiname     string
	port        string
	initialized bool
	server      *http.Server
	localAddr   string
	eventMap    upnpEventMap
	subscrChan  chan *upnpEventRecord
	unpackChan  chan *upnpEvent
	eventChan   chan Event
}

func (this *upnpDefaultReactor) serve() {
	log.Fatal(this.server.ListenAndServe())
}

func (this *upnpDefaultReactor) Init(ifiname, port string) {
	if this.initialized {
		panic("Attempt to reinitialize reactor")
	}

	ifi, err := net.InterfaceByName(ifiname)
	if err != nil {
		panic(err)
	}
	addrs, err := ifi.Addrs()
	if err != nil {
		panic(err)
	}
	ipv4s := []net.Addr{}
	for _, addr := range addrs {
		if addr.(*net.IPNet).IP.To4() != nil {
			ipv4s = append(ipv4s, addr)
		}
	}
	if len(ipv4s) > 0 {
		addrs = ipv4s
	}

	this.initialized = true
	this.port = port
	this.ifiname = ifiname
	this.localAddr = net.JoinHostPort(addrs[0].(*net.IPNet).IP.String(), port)
	this.server = &http.Server{
		Addr:           ":" + port,
		Handler:        nil,
		ReadTimeout:    10 * time.Second,
		WriteTimeout:   10 * time.Second,
		MaxHeaderBytes: 1 << 20,
	}
	http.Handle("/eventSub", this)
	log.Printf("Listening for events on %s", this.localAddr)
	go this.run()
	go this.serve()
}

func (this *upnpDefaultReactor) handleAck(svc *Service, resp *http.Response) (sid string, err error) {
	if resp.Status == "200 OK" {
		sid_key := http.CanonicalHeaderKey("sid")
		if sid_list, has := resp.Header[sid_key]; has {
			sid = sid_list[0]
			svc.ssid = sid // Save sid in the service for re-subscribe

			// USE Timeout value returned to schedule reSubscribe
			sid_timeout := http.CanonicalHeaderKey("TIMEOUT")
			if timeout_list, has := resp.Header[sid_timeout]; has {
				timeoutStr := timeout_list[0]
				if strings.HasPrefix(timeoutStr, "Second-") {
					timeoutStr = strings.TrimPrefix(timeoutStr, "Second-")
				}
				var timeoutVal uint64
				timeoutVal, err = strconv.ParseUint(timeoutStr, 10, 16)
				if err == nil {
					svc.timeout = time.Duration(timeoutVal) * time.Second
					//DBG: log.Printf("Sub_Timeout:%d Sec (%s)\n", timeoutVal, svc.timeout.String())
				} else {
					err = errors.New("subscription ack timeout failed to parse")
				}
			} else {
				err = errors.New("subscription ack missing timeout")
			}
		} else {
			err = errors.New("subscription ack missing sid. ")
		}
	} else {
		errStr := fmt.Sprintln("subscription ack failed: ", resp.Status)
		err = errors.New(errStr)
	}
	return
}

func (this *upnpDefaultReactor) Subscribe(svc *Service, factory EventFactory) (err error) {
	rec := upnpEventRecord{
		svc:     svc,
		factory: factory,
	}
	this.subscrChan <- &rec
	return
}

func (this *upnpDefaultReactor) Channel() chan Event {
	return this.eventChan
}

func (this *upnpDefaultReactor) subscribeImpl(rec *upnpEventRecord) (err error) {
	client := &http.Client{}
	req, err := http.NewRequest("SUBSCRIBE", rec.svc.eventSubURL.String(), nil)
	if nil != err {
		return
	}
	req.Header.Add("HOST", rec.svc.eventSubURL.Host)
	req.Header.Add("USER-AGENT", "unix/5.1 UPnP/1.1 sonos.go/1.0")
	req.Header.Add("CALLBACK", fmt.Sprintf("<http://%s/eventSub>", this.localAddr))
	req.Header.Add("NT", "upnp:event") //Required. Field value contains Notification Type. shall be upnp:event.
	// (No SID header field is used to subscribe.)
	req.Header.Add("TIMEOUT", "Second-1800") //Recommended. Field value contains requested duration until subscription expires. Consists of the keyword Second- followed (without an intervening space)
	var resp *http.Response
	if resp, err = client.Do(req); nil == err {
		defer resp.Body.Close()
		var sid string
		if sid, err = this.handleAck(rec.svc, resp); nil == err {
			this.eventMap[sid] = rec // Save this subscription to the map
		} else {
			log.Println("error on subscription ack:", err)
		}
	} else {
		log.Println("error subscribing:", req.URL, req.Header, err)
	}
	return
}

func (this *upnpDefaultReactor) reSubscribeImpl(rec *upnpEventRecord) (err error) {
	client := &http.Client{}
	req, err := http.NewRequest("SUBSCRIBE", rec.svc.eventSubURL.String(), nil)
	if nil != err {
		return
	}
	req.Header.Add("HOST", rec.svc.eventSubURL.Host) // Required. Field value contains domain name or IP address and optional port components of fully qualified event subscription URL
	req.Header.Add("USER-AGENT", "unix/5.1 UPnP/1.1 sonos.go/1.0")
	// (No CALLBACK header field is used to renew an event subscription.)
	// (No NT header field is used to renew an event subscription.)
	req.Header.Add("SID", rec.svc.ssid)      // Required. Field value contains Subscription Identifier. Shall be the subscription identifier assigned by publisher in response to subscription request. Shall be universally unique. Shall begin with uuid:
	req.Header.Add("TIMEOUT", "Second-1800") // Recommended. Field value contains requested duration until subscription expires. Consists of the keyword Second- followed (without an intervening space)

	var resp *http.Response
	if resp, err = client.Do(req); nil == err {
		defer resp.Body.Close()
		var sid string
		if sid, err = this.handleAck(rec.svc, resp); nil == err {
			this.eventMap[sid] = rec // Update the map
		} else {
			log.Println("error on re-subscribe ack:", err)
			//Clear eventMap info to try full subscribe
			rec.svc.ssid = sid  // if err, sid will be empty (try FullSubscribe later)
			rec.svc.timeout = 0 // use default re-subscribe timeout
		}
	}

	//IF NO Re-Subscription
	if rec.svc.ssid == "" {
		log.Println("error reSubscribing:", req.URL, req.Header, err)

		// TRY NEW SUBSCRIPTION to recover
		err = this.subscribeImpl(rec)
		if err != nil {
			log.Println("error during Subscribe recovery attempt!", err)
			rec.svc.ssid = ""   // Clear the sid so we can try again later
			rec.svc.timeout = 0 // use default re-subscribe timeout
		}
	}
	return
}

func (this *upnpDefaultReactor) unSubscribeImpl(rec *upnpEventRecord) (err error) {
	client := &http.Client{}
	req, err := http.NewRequest("UNSUBSCRIBE", rec.svc.eventSubURL.String(), nil)
	if nil != err {
		return
	}
	req.Header.Add("HOST", rec.svc.eventSubURL.Host)
	req.Header.Add("USER-AGENT", "unix/5.1 UPnP/1.1 sonos.go/1.0")
	// (No CALLBACK header field is used to cancel an event subscription.)
	// (No NT header field is used to renew an event subscription.)
	req.Header.Add("SID", rec.svc.ssid) // Required. Field value contains Subscription Identifier. Shall be the subscription identifier assigned by publisher in response to subscription request. Shall be universally unique. Shall begin with uuid:
	// (No TIMEOUT header field is used to cancel an event subscription.)

	var resp *http.Response
	if resp, err = client.Do(req); nil == err {
		defer resp.Body.Close()
		sid := rec.svc.ssid
		this.eventMap[sid] = rec
	} else {
		log.Println("error unSubscribing:", req.URL, req.Header, err)
	}
	return
}

func (this *upnpDefaultReactor) maybePostEvent(event *upnpEvent) {
	if rec, has := this.eventMap[event.sid]; has {
		switch event.etype {
		case upnpEventTypeProperty:
			rec.factory.HandleProperty(rec.svc, event.value, this.eventChan)
		case upnpEventTypeBeginSet:
			rec.factory.BeginSet(rec.svc, this.eventChan)
		case upnpEventTypeEndSet:
			rec.factory.EndSet(rec.svc, this.eventChan)
		}
	}
}

func (this *upnpDefaultReactor) run() {
	var err error
	for {
		select {
		case subscr := <-this.subscrChan:
			if subscr.svc.ssid == "" { // New-Subscription
				err = this.subscribeImpl(subscr)
				//DBG: log.Printf("SUBSCRIBE %s = %s : %d\n", subscr.svc.serviceId, subscr.svc.ssid, subscr.svc.timeout/time.Second)
			} else { // Re-Subscription
				//DBG: log.Printf("RESUBSCRIBE %s : %d\n", subscr.svc.ssid, subscr.svc.timeout/time.Second)
				err = this.reSubscribeImpl(subscr)
			}
			if err != nil {
				log.Println("error during Subscribe:", err)
			}

			// Schedule next re/Subscribe attempt
			go func() {
				resubscribeTime := subscr.svc.timeout / 2 // resubscribe in half the timeout time
				if 0 == subscr.svc.timeout {
					log.Printf("warning: Subscription timeout=0 Use 10min instead\n")
					resubscribeTime = 10 * time.Minute
				}
				resTimer := time.NewTimer(resubscribeTime)
				<-resTimer.C              // waits here for timer to expire
				this.subscrChan <- subscr // send subscription to re-subscribe
			}()
		case event := <-this.unpackChan:
			this.maybePostEvent(event)
		}
	}
}

func (this *upnpDefaultReactor) sendAck(writer http.ResponseWriter) {
	writer.Write(nil)
}

func (this *upnpDefaultReactor) notifyBegin(sid string) {
	event := &upnpEvent{
		sid:   sid,
		etype: upnpEventTypeBeginSet,
	}
	this.unpackChan <- event
}

func (this *upnpDefaultReactor) notify(sid, value string) {
	event := &upnpEvent{
		sid:   sid,
		value: value,
		etype: upnpEventTypeProperty,
	}
	this.unpackChan <- event
}

func (this *upnpDefaultReactor) notifyEnd(sid string) {
	event := &upnpEvent{
		sid:   sid,
		etype: upnpEventTypeEndSet,
	}
	this.unpackChan <- event
}

func (this *upnpDefaultReactor) unpack(sid string, doc *upnpEvent_XML) {
	this.notifyBegin(sid)
	for _, prop := range doc.Properties {
		this.notify(sid, prop.Content)
	}
	this.notifyEnd(sid)
}

func (this *upnpDefaultReactor) handle(request *http.Request) {
	defer request.Body.Close()
	if body, err := io.ReadAll(request.Body); nil != err {
		panic(err)
	} else {
		sid_key := http.CanonicalHeaderKey("sid")
		var sid string
		if sid_list, has := request.Header[sid_key]; has {
			//DBG: log.Println("event xml:", string(body))
			sid = sid_list[0]
			doc := &upnpEvent_XML{}
			xml.Unmarshal(body, doc)
			this.unpack(sid, doc)
		}
	}
}

func (this *upnpDefaultReactor) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	defer this.sendAck(writer)
	this.handle(request)
}

func MakeReactor() Reactor {
	reactor := &upnpDefaultReactor{}
	reactor.eventMap = make(upnpEventMap)
	reactor.subscrChan = make(chan *upnpEventRecord, 1024)
	reactor.unpackChan = make(chan *upnpEvent, 1024)
	reactor.eventChan = make(chan Event, 1024)
	return reactor
}
