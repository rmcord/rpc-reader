package richpresence

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"
)

type PresenceHandler func(update PresenceUpdate)

type Reader struct {
	mu        sync.RWMutex
	listeners []net.Listener
	conns     map[net.Conn]*clientState
	handlers  []PresenceHandler
	running   bool
	stopCh    chan struct{}

	FakeUser *UserData
}

type clientState struct {
	clientID string
	conn     net.Conn
}

func New() *Reader {
	return &Reader{
		conns:  make(map[net.Conn]*clientState),
		stopCh: make(chan struct{}),
		FakeUser: &UserData{
			ID:            "000000000000000000",
			Username:      "hindsight",
			Discriminator: "0000",
		},
	}
}

func (r *Reader) OnPresence(handler PresenceHandler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handlers = append(r.handlers, handler)
}

func (r *Reader) Start() error {
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return fmt.Errorf("reader already running")
	}
	r.running = true
	r.mu.Unlock()

	var started int
	for i := 0; i < 10; i++ {
		ln, err := createListener(i)
		if err != nil {
			log.Printf("could not create IPC listener %d: %v", i, err)
			log.Printf("this may be due to discord being open")
			continue
		}

		r.mu.Lock()
		r.listeners = append(r.listeners, ln)
		r.mu.Unlock()

		go r.acceptLoop(ln, i)
		started++
		log.Printf("listening on discord-ipc-%d", i)
	}

	if started == 0 {
		return fmt.Errorf("failed to create any IPC listeners")
	}

	return nil
}

func (r *Reader) Stop() {
	r.mu.Lock()
	if !r.running {
		r.mu.Unlock()
		return
	}
	r.running = false
	close(r.stopCh)

	for _, ln := range r.listeners {
		ln.Close()
	}
	r.listeners = nil

	for conn := range r.conns {
		conn.Close()
	}
	r.conns = make(map[net.Conn]*clientState)
	r.mu.Unlock()
}

func (r *Reader) acceptLoop(ln net.Listener, idx int) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-r.stopCh:
				return
			default:
				log.Printf("accept error on ipc-%d: %v", idx, err)
				continue
			}
		}

		r.mu.Lock()
		r.conns[conn] = &clientState{conn: conn}
		r.mu.Unlock()

		go r.handleConnection(conn)
	}
}

func (r *Reader) handleConnection(conn net.Conn) {
	defer func() {
		conn.Close()
		r.mu.Lock()
		delete(r.conns, conn)
		r.mu.Unlock()
	}()

	for {
		opcode, payload, err := r.readFrame(conn)
		if err != nil {
			if err != io.EOF {
				log.Printf("read error: %v", err)
			}
			return
		}

		r.handleFrame(conn, opcode, payload)
	}
}

func (r *Reader) readFrame(conn net.Conn) (uint32, []byte, error) {
	header := make([]byte, 8)
	if _, err := io.ReadFull(conn, header); err != nil {
		return 0, nil, err
	}

	opcode := binary.LittleEndian.Uint32(header[0:4])
	length := binary.LittleEndian.Uint32(header[4:8])

	if length > 1024*1024 { // 1MB max (prevents abuse)
		return 0, nil, fmt.Errorf("payload too large: %d", length)
	}

	payload := make([]byte, length)
	if _, err := io.ReadFull(conn, payload); err != nil {
		return 0, nil, err
	}

	return opcode, payload, nil
}

func (r *Reader) writeFrame(conn net.Conn, opcode uint32, data interface{}) error {
	payload, err := json.Marshal(data)
	if err != nil {
		return err
	}

	header := make([]byte, 8)
	binary.LittleEndian.PutUint32(header[0:4], opcode)
	binary.LittleEndian.PutUint32(header[4:8], uint32(len(payload)))

	if _, err := conn.Write(header); err != nil {
		return err
	}
	if _, err := conn.Write(payload); err != nil {
		return err
	}

	return nil
}

func (r *Reader) handleFrame(conn net.Conn, opcode uint32, payload []byte) {
	switch opcode {
	case OpHandshake:
		r.handleHandshake(conn, payload)
	case OpFrame:
		r.handleMessage(conn, payload)
	case OpClose:
		conn.Close()
	case OpPing:
		r.writeFrame(conn, OpPong, json.RawMessage(payload))
	}
}

func (r *Reader) handleHandshake(conn net.Conn, payload []byte) {
	var hs Handshake
	if err := json.Unmarshal(payload, &hs); err != nil {
		log.Printf("invalid handshake: %v", err)
		return
	}

	r.mu.Lock()
	if state, ok := r.conns[conn]; ok {
		state.clientID = hs.ClientID
	}
	r.mu.Unlock()

	log.Printf("handshake from client_id: %s (v%d)", hs.ClientID, hs.V)

	// Send READY response, to send user info
	response := Frame{
		Cmd: CmdDispatch,
		Evt: EvtReady,
		Data: ReadyData{
			V:    1,
			User: r.FakeUser,
		},
	}

	r.writeFrame(conn, OpFrame, response)
}

func (r *Reader) handleMessage(conn net.Conn, payload []byte) {
	var frame Frame
	if err := json.Unmarshal(payload, &frame); err != nil {
		log.Printf("Invalid frame: %v", err)
		return
	}

	switch frame.Cmd {
	case CmdSetActivity:
		r.handleSetActivity(conn, frame)
	default:
		// for unhandled commands just send back a generic response
		// seems to work, needs more work for other types to ensure it doesnt reject / cause errors
		response := Frame{
			Cmd:   CmdDispatch,
			Nonce: frame.Nonce,
			Evt:   frame.Cmd,
			Data:  map[string]interface{}{},
		}
		r.writeFrame(conn, OpFrame, response)
	}
}

func (r *Reader) handleSetActivity(conn net.Conn, frame Frame) {
	r.mu.RLock()
	state := r.conns[conn]
	clientID := ""
	if state != nil {
		clientID = state.clientID
	}
	handlers := make([]PresenceHandler, len(r.handlers))
	copy(handlers, r.handlers)
	r.mu.RUnlock()

	argsBytes, _ := json.Marshal(frame.Args)
	var args SetActivityArgs
	json.Unmarshal(argsBytes, &args)

	update := PresenceUpdate{
		ClientID:  clientID,
		Activity:  args.Activity,
		PID:       args.PID,
		Timestamp: time.Now(),
	}

	if args.Activity != nil {
		log.Printf("activity update [%s]: %s - %s",
			clientID,
			args.Activity.Details,
			args.Activity.State,
		)
	} else {
		log.Printf("activity cleared [%s]", clientID)
	}

	for _, handler := range handlers {
		go handler(update)
	}

	// send success
	response := Frame{
		Cmd:   CmdDispatch,
		Nonce: frame.Nonce,
		Evt:   CmdSetActivity,
		Data:  args.Activity,
	}
	r.writeFrame(conn, OpFrame, response)
}

// sends a list of active ids that have sent an update
func (r *Reader) GetActiveClients() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	clients := make([]string, 0, len(r.conns))
	for _, state := range r.conns {
		if state.clientID != "" {
			clients = append(clients, state.clientID)
		}
	}
	return clients
}
