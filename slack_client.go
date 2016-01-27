package go_slack

import (
	"encoding/json"
	"fmt"
	"golang.org/x/net/websocket"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Packet map[string]interface{}

type UserType map[string]interface{}
type ChannelType map[string]interface{}

func (u UserType) Name() string {
	return u["name"].(string)
}

func (c ChannelType) Name() string {
	return c["name"].(string)
}

type SlackConn struct {
	auth_token string
	send       chan Packet
	recv       chan Packet
	pong       chan Packet
	done       chan struct{}

	connected bool
	wg        sync.WaitGroup
	ws        *websocket.Conn
	mu        sync.Mutex

	handler *SlackEventMux

	// Channel and UserInfo
	Users    map[string]UserType
	Channels map[string]ChannelType

	// Packet ID
	packetId uint32
}

type RequestParam struct {
	url.Values
}

func MakeParams(params Packet) RequestParam {
	p := RequestParam{url.Values{}}
	for k, v := range params {
		p.Set(k, fmt.Sprintf("%v", v))
	}
	return p
}

func NewSlackConn(auth_token string) *SlackConn {
	s := &SlackConn{
		auth_token: auth_token,
		handler:    &SlackEventMux{m: make(map[string][]SlackHandler)},
	}

	// default handler
	defaultHandler := map[string]SlackHandlerFunc{
		"pong":        (*SlackConn).onPong,
		"team_join":   (*SlackConn).onTeamJoin,
		"user_change": (*SlackConn).onUserChange,
	}

	for c, h := range defaultHandler {
		s.handler.Handle(c, h)
	}
	return s
}

func (s *SlackConn) SendAPI(method string, params Packet) (map[string]interface{}, error) {
	p := MakeParams(params)
	p.Set("token", s.auth_token)
	resp, err := http.PostForm("https://slack.com/api/"+method, p.Values)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(resp.Body)
	defer resp.Body.Close()
	var respObj map[string]interface{}
	err = decoder.Decode(&respObj)
	if err != nil {
		return nil, err
	}
	return respObj, nil
}

func (s *SlackConn) Connect() error {
	resp, err := s.SendAPI("rtm.start", Packet{})
	if err != nil {
		return err
	}
	if !resp["ok"].(bool) {
		return fmt.Errorf("Start Session Error: %s", resp["error"].(string))
	}

	// Initialize Session and UserInfo
	s.Users = make(map[string]UserType)
	s.Channels = make(map[string]ChannelType)
	for _, obj := range resp["users"].([]interface{}) {
		u := obj.(map[string]interface{})
		s.Users[u["id"].(string)] = u
	}
	for _, obj := range resp["channels"].([]interface{}) {
		c := obj.(map[string]interface{})
		s.Channels[c["id"].(string)] = c
	}

	ws_url := resp["url"].(string)
	u, err := url.Parse(ws_url)
	if err != nil {
		return err
	}
	origin := fmt.Sprintf("%s://%s", u.Scheme, u.Host)
	s.ws, err = websocket.Dial(ws_url, "", origin)
	if err != nil {
		log.Println(err)
		return err
	}

	// Initialize variables related to connection
	s.send = make(chan Packet)
	s.recv = make(chan Packet)
	s.pong = make(chan Packet)
	s.done = make(chan struct{})
	s.packetId = 0

	s.mu.Lock()
	s.connected = true
	s.mu.Unlock()
	s.wg.Add(3)
	go s.reader()
	go s.writer()
	go s.ping()
	s.handler.dispatch(s, Packet{"type": "connected"})
	return nil
}

func (s *SlackConn) getPacketId() uint32 {
	return atomic.AddUint32(&s.packetId, 1)
}

func (s *SlackConn) ping() {
	tick := time.NewTicker(10 * time.Second)
	stop := make(chan struct{})
	for {
		select {
		case <-tick.C:
			packetId := s.getPacketId()
			s.send <- Packet{
				"id":   packetId,
				"type": "ping",
			}
			go func() {
				select {
				case packet, ok := <-s.pong:
					if uint32(packet["reply_to"].(float64)) != packetId || !ok {
						close(stop)
					}
				case <-time.After(10 * time.Second):
					log.Println("ping timeout!")
					close(stop)
				}
			}()
		case <-s.done:
			s.wg.Done()
			s.shutdown()
			return
		case <-stop:
			s.wg.Done()
			s.shutdown()
			return
		}
	}
}

func (s *SlackConn) reader() {
	for {
		go func() {
			var packet Packet
			err := websocket.JSON.Receive(s.ws, &packet)
			if err != nil {
				log.Println(err)
				s.wg.Done()
				s.shutdown()
				return
			}
			s.recv <- packet
		}()
		select {
		case packet := <-s.recv:
			log.Println(packet)
			go s.handler.dispatch(s, packet)
		case <-s.done:
			return
		}
	}
}

func (s *SlackConn) writer() {
	stop := make(chan struct{})
	for {
		select {
		case packet := <-s.send:
			s.ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
			err := websocket.JSON.Send(s.ws, packet)
			if err != nil {
				close(stop)
			}
		case <-s.done:
			s.wg.Done()
			s.shutdown()
			return
		case <-stop:
			s.wg.Done()
			s.shutdown()
			return
		}
	}
}

func (s *SlackConn) shutdown() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.connected {
		return
	}

	s.connected = false
	s.ws.Close()
	close(s.done)
	s.wg.Wait()
	s.handler.dispatch(s, Packet{"type": "disconnected"})
}

func (s *SlackConn) Handle(cmd string, handler SlackHandler) {
	s.handler.Handle(cmd, handler)
}

func (s *SlackConn) HandleFunc(cmd string, handler func(conn *SlackConn, params Packet)) {
	s.Handle(cmd, SlackHandlerFunc(handler))
}

func (s *SlackConn) onPong(params Packet) {
	s.pong <- params
}

func (s *SlackConn) onTeamJoin(params Packet) {
	user := params["user"].(map[string]interface{})
	s.Users[user["id"].(string)] = user
}

func (s *SlackConn) onUserChange(params Packet) {
	user := params["user"].(map[string]interface{})
	s.Users[user["id"].(string)] = user
}

func (s *SlackConn) SendMessage(username, message, channel string) error {
	if !s.connected {
		return fmt.Errorf("slack is not connected")
	}

	if _, ok := s.Channels[channel]; !ok {
		return fmt.Errorf("There is no such channel (%s)", channel)
	}

	params := Packet{
		"channel":    channel,
		"text":       message,
		"username":   username,
		"link_names": 1,
		"as_user":    false,
	}
	/*
		s.send <- Packet{
			"id":         s.getPacketId(),
			"type":       "message",
			"channel":    channel,
			"text":       message,
			"username":   username,
			"link_names": 1,
			"parse":      "full",
			"as_user":    false,
		}
		return nil
	*/

	respObj, err := s.SendAPI("chat.postMessage", params)
	log.Println(respObj)
	return err
}

func (s *SlackConn) ParseMessage(message string) string {
	re := regexp.MustCompile(`<(.*?)>`)
	out := re.ReplaceAllStringFunc(message, func(in string) string {
		in = in[1 : len(in)-1]
		if len(in) < 2 {
			return in
		}
		fields := strings.Split(in, "|")
		if len(fields) == 2 {
			return fields[1]
		}
		switch {
		case in[:2] == "@U":
			return "@" + s.Users[in[1:]].Name()
		case in[:2] == "#C":
			return "#" + s.Channels[in[1:]].Name()
		case in[:1] == "!":
			return in
		}
		return in
	})
	out = strings.Replace(out, "&amp;", "&", -1)
	out = strings.Replace(out, "&lt;", "<", -1)
	out = strings.Replace(out, "&gt;", ">", -1)
	return out
}
