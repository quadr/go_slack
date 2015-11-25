package go_slack

import (
	"sync"
)

type SlackHandler interface {
	Handle(conn *SlackConn, params Packet)
}

type SlackHandlerFunc func(*SlackConn, Packet)

func (h SlackHandlerFunc) Handle(conn *SlackConn, params Packet) {
	h(conn, params)
}

type SlackEventMux struct {
	sync.RWMutex
	m map[string][]SlackHandler
}

func (mux *SlackEventMux) Handle(cmd string, handler SlackHandler) {
	mux.Lock()
	defer mux.Unlock()
	if _, ok := mux.m[cmd]; !ok {
		mux.m[cmd] = []SlackHandler{}
	}

	mux.m[cmd] = append(mux.m[cmd], handler)
}

func (mux *SlackEventMux) HandleFunc(cmd string, handler func(conn *SlackConn, params Packet)) {
	mux.Handle(cmd, SlackHandlerFunc(handler))
}

func (mux *SlackEventMux) dispatch(conn *SlackConn, params Packet) {
	mux.RLock()
	defer mux.RUnlock()
	cmd, ok := params["type"].(string)
	if !ok {
		return
	}
	for _, handler := range mux.m[cmd] {
		handler.Handle(conn, params)
	}
}
