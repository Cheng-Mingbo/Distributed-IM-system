package server

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
	"github.com/sirupsen/logrus"
)

type ServerOptions struct {
	writewait time.Duration // 写超时时间
	readwait  time.Duration // 读超时时间
}

type Server struct {
	once       sync.Once           // 保证只执行一次
	options    ServerOptions       // 服务器配置
	id         string              // 服务器ID
	address    string              // 服务器地址
	sync.Mutex                     // 互斥锁
	users      map[string]net.Conn // 会话列表
}

func newServer(id, address string) *Server {
	return &Server{
		id:      id,
		address: address,
		users:   make(map[string]net.Conn, 100),
		options: ServerOptions{
			writewait: time.Second * 10,
			readwait:  time.Minute * 2,
		},
	}
}

// NewServer 创建一个服务器
func NewServer(id, address string) *Server {
	return newServer(id, address)
}

func (s *Server) Start() error {
	mux := http.NewServeMux()
	log := logrus.WithFields(logrus.Fields{
		"module": "Server",
		"listen": s.address,
		"id":     s.id,
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		conn, _, _, err := ws.UpgradeHTTP(r, w)
		if err != nil {
			conn.Close()
			return
		}
		// 读取uid
		user := r.URL.Query().Get("user")
		if user == "" {
			conn.Close()
			return
		}

		// 保存会话，如果用户已经存在，则关闭之前的连接
		old, ok := s.addUser(user, conn)
		if ok {
			old.Close()
			log.Infof("user %s already exists, close old connection", user)
		}
		log.Infof("user %s connected", user)

		go func(user string, conn net.Conn) {
			err := s.readloop(user, conn)
			if err != nil {
				log.Warn("readloop error:", err)
			}
			conn.Close()
			s.delUser(user)
			log.Infof("user %s disconnected", user)
		}(user, conn)
	})
	log.Infoln("server start")
	return http.ListenAndServe(s.address, mux)
}

// addUser 添加用户
func (s *Server) addUser(user string, conn net.Conn) (net.Conn, bool) {
	s.Lock()
	defer s.Unlock()
	old, ok := s.users[user] // 旧的连接
	s.users[user] = conn
	return old, ok
}

// delUser 删除用户
func (s *Server) delUser(user string) {
	s.Lock()
	defer s.Unlock()
	delete(s.users, user)
}

// Shutdown 关闭服务器
func (s *Server) Shutdown() {
	s.once.Do(func() {
		s.Lock()
		defer s.Unlock()
		for _, conn := range s.users {
			conn.Close()
		}
	})
}

// readloop 读取数据
func (s *Server) readloop(user string, conn net.Conn) error {
	for {
		_ = conn.SetReadDeadline(time.Now().Add(s.options.readwait)) // 设置读超时

		frame, err := ws.ReadFrame(conn)
		if err != nil {
			return err
		}
		if frame.Header.Masked {
			ws.Cipher(frame.Payload, frame.Header.Mask, 0)
		}
		switch frame.Header.OpCode {
		case ws.OpPing:
			// 心跳
			_ = wsutil.WriteServerMessage(conn, ws.OpPong, nil)
			logrus.Info("write pong")
			continue
		case ws.OpClose:
			// 关闭
			return errors.New("remote close")
		case ws.OpText:
			// 文本
			go s.handle(user, string(frame.Payload))
		case ws.OpBinary:
			go s.handleBinary(user, frame.Payload)
		}
	}
}

// handle 处理文本消息
func (s *Server) handle(user, msg string) {
	logrus.Infof("user %s send message: %s", user, msg)
	s.Lock()
	defer s.Unlock()
	broadcast := fmt.Sprintf("%s -- From %s", msg, user)
	for u, conn := range s.users {
		if u == user {
			continue
		}
		logrus.Infof("send message to %s: %s", u, broadcast)
		err := s.writeText(conn, broadcast)
		if err != nil {
			logrus.Errorf("send message to %s error: %s", u, err)
		}
	}
}

const (
	CommandPing = 100
	CommandPong = 101
)

// handleBinary 处理二进制消息
func (s *Server) handleBinary(user string, msg []byte) {
	logrus.Infof("user %s send binary message: %v", user, msg)
	s.Lock()
	defer s.Unlock()
	// handle ping request
	i := 0
	command := binary.BigEndian.Uint16(msg[i : i+2])
	i += 2
	payloadLen := binary.BigEndian.Uint32(msg[i : i+4])
	logrus.Infof("command: %d, payloadLen: %d", command, payloadLen)
	if command == CommandPing {
		u := s.users[user]
		err := wsutil.WriteServerBinary(u, []byte{CommandPong, 0, 0, 0, 0})
		if err != nil {
			logrus.Errorf("send pong to %s error: %s", user, err)
		}
	}
}

// writeText 写入文本消息
func (s *Server) writeText(conn net.Conn, msg string) error {
	f := ws.NewTextFrame([]byte(msg))
	err := conn.SetWriteDeadline(time.Now().Add(s.options.writewait))
	if err != nil {
		return err
	}
	return ws.WriteFrame(conn, f)
}
