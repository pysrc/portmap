package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"os"
	"os/signal"
	"pmap/encrypto"
	"sync"
	"syscall"
	"time"
)

// ServerConfig 服务端配置
type ServerConfig struct {
	Key       string   `json:"key"`         // 配对密码
	Port      uint16   `json:"port"`        // 控制监听端口
	LimitPort []uint16 `json:"-limit-port"` // 开端口范围
}

// ClientMapConfig 客户端map配置
type ClientMapConfig struct {
	Inner string `json:"inner"`
	Outer uint16 `json:"outer"`
}

// ClientConfig 客户端配置
type ClientConfig struct {
	Key    string            `json:"key"`
	Server string            `json:"server"`
	Map    []ClientMapConfig `json:"map"`
}

// Config 配置
type Config struct {
	Server *ServerConfig `json:"server"`
	Client *ClientConfig `json:"client"`
}

const (
	_ uint8 = iota
	// START 第一次连接服务器
	START
	// NEWSOCKET 新连接
	NEWSOCKET
	// NEWCONN 新连接发送到服务端命令
	NEWCONN
	// ERROR 处理失败
	ERROR
	// SUCCESS 处理成功
	SUCCESS
	// IDLE 空闲命令 什么也不做
	IDLE
	// KILL 退出命令
	KILL
	// ERROR_PWD 密码错误
	ERROR_PWD
	// ERROR_BUSY 端口被占用
	ERROR_BUSY
	// ERROR_LIMIT_PORT 不满足端口范围
	ERROR_LIMIT_PORT
)

const (
	// RetryTime 断线重连时间
	RetryTime          = time.Second
	TcpKeepAlivePeriod = 30 * time.Second
	WaitTimeOut        = 30 * time.Second // 连接等待超时时间
	WaitMax            = 10
)

func Recover() {
	if err := recover(); err != nil {
		log.Println(err)
	}
}

type Worker struct {
	Conn     net.Conn // 客户端连接
	LastTime int64    // 客户端连接超时时间
}

type Resource struct {
	Listener   net.Listener
	WaitWorker [WaitMax]*Worker // 工作负载
	Running    bool
	mu         sync.Mutex // 工作负载锁
}

// 新连接
func (r *Resource) NewConn(conn net.Conn) (bool, uint8) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, v := range r.WaitWorker {
		if v == nil {
			r.WaitWorker[i] = &Worker{
				Conn:     conn,
				LastTime: time.Now().Add(WaitTimeOut).Unix(),
			}
			return true, uint8(i)
		}
		// 超时
		if time.Now().Unix() > v.LastTime {
			v.Conn.Close()
			r.WaitWorker[i] = &Worker{
				Conn:     conn,
				LastTime: time.Now().Add(WaitTimeOut).Unix(),
			}
			return true, uint8(i)
		}
	}
	return false, 0
}

// DoServer 服务端处理
func DoServer(config *ServerConfig) {
	if config == nil {
		return
	}
	lis, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%v", config.Port))
	if err != nil {
		log.Println("Initialization error", err)
		return
	}
	defer lis.Close()
	// 端口-资源对应
	var resourceMap = make(map[uint16]*Resource)
	var resourceMu sync.Mutex
	// 处理对客户端的监听
	var dolisten = func(ctx context.Context, cconn net.Conn, port uint16) {
		if resourceMap[port] == nil {
			return
		}
		defer func() {
			Recover()
			defer Recover()
			rs := resourceMap[port]
			if rs == nil {
				return
			}
			for _, v := range rs.WaitWorker {
				if v != nil && v.Conn != nil {
					v.Conn.Close()
				}
			}
			rs.Listener.Close()
			resourceMu.Lock()
			delete(resourceMap, port)
			resourceMu.Unlock()
			log.Println("Close port:", port)
		}()
		log.Println("Open port:", port)
		var rsc = resourceMap[port]
		go func() {
			defer Recover()
			for {
				outcon, err := rsc.Listener.Accept()
				if err != nil {
					return
				}
				// 通知客户端建立连接
				ok, id := rsc.NewConn(outcon)
				if ok {
					var buffer bytes.Buffer
					buffer.Write([]byte{NEWSOCKET})
					buffer.Write([]byte{uint8(port >> 8), uint8(port & 0xff)})
					buffer.Write([]byte{id})
					opencmd := buffer.Bytes()
					buffer.Reset()
					cconn.Write(opencmd)
				} else {
					outcon.Close()
				}

			}
		}()
		<-ctx.Done()
	}
	// 处理客户端新连接
	var doconn = func(conn net.Conn) {
		defer Recover()
		var cmd = make([]byte, 1)
		if _, err = io.ReadAtLeast(conn, cmd, 1); err != nil {
			conn.Close()
			return
		}
		switch cmd[0] {
		case START:
			defer conn.Close()
			// 初始化
			// START info_len info
			info_len := make([]byte, 8)
			if _, err = io.ReadAtLeast(conn, info_len, 8); err != nil {
				return
			}
			var ilen = (uint64(info_len[0]) << 56) | (uint64(info_len[1]) << 48) | (uint64(info_len[2]) << 40) | (uint64(info_len[3]) << 32) | (uint64(info_len[4]) << 24) | (uint64(info_len[5]) << 16) | (uint64(info_len[6]) << 8) | (uint64(info_len[7]))
			if ilen > 1024*1024 {
				// 限制消息最大内存使用量 1M
				return
			}
			var clinfo = make([]byte, ilen)
			if _, err = io.ReadAtLeast(conn, clinfo, int(ilen)); err != nil {
				return
			}
			var clicfg ClientConfig
			if nil != json.Unmarshal(clinfo, &clicfg) {
				return
			}
			if clicfg.Key != config.Key {
				conn.Write([]byte{ERROR_PWD})
				return
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			// 打开端口
			for _, cc := range clicfg.Map {
				// 判断端口是否合法
				if len(config.LimitPort) >= 2 {
					if cc.Outer < config.LimitPort[0] || cc.Outer > config.LimitPort[1] {
						// 不满足端口范围
						log.Printf("Does not meet the port range[%v, %v] %v", config.LimitPort[0], config.LimitPort[1], cc.Outer)
						conn.Write([]byte{ERROR_LIMIT_PORT})
						return
					}
				}
				clis, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%v", cc.Outer))
				if err != nil {
					log.Println("Port is occupied", cc.Outer)
					conn.Write([]byte{ERROR_BUSY})
					return
				}
				resourceMu.Lock()
				resourceMap[cc.Outer] = &Resource{
					Listener: clis,
					Running:  true,
				}
				resourceMu.Unlock()
				go dolisten(ctx, conn, cc.Outer)
			}
			conn.Write([]byte{SUCCESS})
			for {
				n, err := conn.Read(cmd)
				if err != nil {
					return
				}
				if n != 0 {
					switch cmd[0] {
					case KILL:
						return
					case IDLE:
						continue
					}
				}
			}
		case NEWCONN:
			// 客户端新建立连接
			sport := make([]byte, 3)
			io.ReadAtLeast(conn, sport, 3)
			pt := (uint16(sport[0]) << 8) + uint16(sport[1])
			id := uint8(sport[2])
			client := resourceMap[pt]
			if client != nil {
				if int(id) >= len(client.WaitWorker) {
					conn.Close()
					return
				} else {
					wk := client.WaitWorker[id]
					if wk == nil {
						conn.Close()
						return
					}
					if wk.LastTime < time.Now().Unix() {
						if wk.Conn != nil {
							wk.Conn.Close()
						}
						conn.Close()
						return
					}
					var s encrypto.NCopy
					key, iv := encrypto.GetKeyIv(config.Key)
					s.Init(conn, key, iv)
					go encrypto.WCopy(&s, wk.Conn)
					go encrypto.RCopy(wk.Conn, &s)
					client.mu.Lock()
					defer client.mu.Unlock()
					client.WaitWorker[id] = nil
				}
			} else {
				conn.Close()
			}
		default:
			conn.Close()
		}
	}

	for {
		remoteConn, err := lis.Accept()
		if err != nil {
			log.Println(err)
			continue
		}
		go doconn(remoteConn)
	}

}

// DoClient 客户端处理
func DoClient(config *ClientConfig) {
	if config == nil {
		return
	}
	var portmap = make(map[uint16]string, len(config.Map))
	for _, m := range config.Map {
		portmap[m.Outer] = m.Inner
	}
	var isContinue = true
	// 新建连接处理
	var doconn = func(conn net.Conn, sport uint16, sp []byte) {
		defer Recover()
		localConn, err := net.Dial("tcp", portmap[sport])
		if err != nil {
			conn.Close()
			log.Println(err)
			return
		}
		conn.Write([]byte{NEWCONN})
		conn.Write(sp)
		key, iv := encrypto.GetKeyIv(config.Key)
		var s encrypto.NCopy
		s.Init(conn, key, iv)
		go encrypto.WCopy(&s, localConn)
		go encrypto.RCopy(localConn, &s)
	}
	for isContinue {
		func() {
			defer Recover()
			defer time.Sleep(RetryTime)
			log.Println("Connecting to server...")
			serverConn, err := net.Dial("tcp", config.Server)
			if err != nil {
				log.Println("Can't connect to server")
				return
			}
			defer serverConn.Close()
			serverConn.(*net.TCPConn).SetKeepAlive(true)
			serverConn.(*net.TCPConn).SetKeepAlivePeriod(TcpKeepAlivePeriod)
			clinfo, _ := json.Marshal(config)
			// 添加字节缓冲
			var buffer bytes.Buffer
			// 发送客户端信息
			// START info_len info
			buffer.Write([]byte{START})
			binary.Write(&buffer, binary.BigEndian, uint64(len(clinfo)))
			buffer.Write(clinfo)
			serverConn.Write(buffer.Bytes())
			buffer.Reset()
			// 读取返回信息
			// SUCCESS / ERROR / BUSY
			var recvcmd = make([]byte, 1)
			io.ReadAtLeast(serverConn, recvcmd, 1)
			switch recvcmd[0] {
			case ERROR_PWD:
				log.Println("Wrong password")
				isContinue = false
				return
			case ERROR_BUSY:
				log.Println("Port is occupied")
				isContinue = false
				return
			case ERROR_LIMIT_PORT:
				log.Println("Does not meet the port range")
				isContinue = false
				return
			}
			if recvcmd[0] != SUCCESS {
				// 密码错误
				log.Println("Unknown error")
				isContinue = false
				return
			}
			log.Println("Certification successful")
			for _, cc := range config.Map {
				log.Printf("%v->:%v\n", cc.Inner, cc.Outer)
			}
			recvcmd[0] = IDLE
			// 进入指令读取循环
			for {
				_, err = serverConn.Read(recvcmd)
				if err != nil {
					return
				}
				switch recvcmd[0] {
				case NEWSOCKET:
					// 新建连接
					// 读取远端端口与id
					sp := make([]byte, 3)
					io.ReadAtLeast(serverConn, sp, 3)
					sport := uint16(sp[0])<<8 + uint16(sp[1])
					conn, err := net.Dial("tcp", config.Server)
					if err != nil {
						return
					}
					go doconn(conn, sport, sp)
				case IDLE:
					_, err := serverConn.Write([]byte{SUCCESS})
					if err != nil {
						return
					}
				}
			}
		}()
	}
}

func main() {
	cfg := flag.String("f", "config.json", "Config file")
	flag.Parse()
	psignal := make(chan os.Signal, 1)
	// ctrl+c->SIGINT, kill -9 -> SIGKILL
	signal.Notify(psignal, syscall.SIGINT, syscall.SIGTERM)
	configBytes, err := ioutil.ReadFile(*cfg)
	if err != nil {
		panic(err)
	}
	var config Config
	err = json.Unmarshal(configBytes, &config)
	if err != nil {
		panic(err)
	}
	go DoServer(config.Server)
	go DoClient(config.Client)
	<-psignal
	log.Println("Bye~")
}
