package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// ServerConfig 服务端配置
type ServerConfig struct {
	Key  string `json:"key"`
	Port uint16 `json:"port"`
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
)

const (
	// RetryTime 断线重连时间
	RetryTime          = time.Second
	TcpKeepAlivePeriod = 30 * time.Second
)

func Recover() {
	if err := recover(); err != nil {
		log.Println(err)
	}
}

type Resource struct {
	Listener net.Listener
	ConnChan chan net.Conn
	Running  bool
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
			close(resourceMap[port].ConnChan)
			resourceMap[port].Listener.Close()
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
				cconn.Write([]byte{NEWSOCKET})
				cconn.Write([]byte{uint8(port >> 8), uint8(port & 0xff)})
				rsc.ConnChan <- outcon
			}
		}()
		<-ctx.Done()
	}
	// 处理客户端新连接
	var doconn = func(conn net.Conn) {
		defer Recover()
		defer conn.Close()
		var cmd = make([]byte, 1)
		if _, err = io.ReadAtLeast(conn, cmd, 1); err != nil {
			return
		}
		switch cmd[0] {
		case START:
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
				conn.Write([]byte{ERROR})
				return
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			// 打开端口
			for _, cc := range clicfg.Map {
				clis, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%v", cc.Outer))
				if err != nil {
					log.Println("Open port error", err)
					conn.Write([]byte{ERROR})
					return
				}
				resourceMu.Lock()
				resourceMap[cc.Outer] = &Resource{
					Listener: clis,
					ConnChan: make(chan net.Conn),
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
			sport := make([]byte, 2)
			io.ReadAtLeast(conn, sport, 2)
			pt := (uint16(sport[0]) << 8) + uint16(sport[1])
			client := resourceMap[pt]
			if client != nil {
				if rconn, ok := <-client.ConnChan; ok {
					go io.Copy(rconn, conn)
					io.Copy(conn, rconn)
					conn.Close()
					rconn.Close()
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
		defer conn.Close()
		localConn, err := net.Dial("tcp", portmap[sport])
		localConn.(*net.TCPConn).SetKeepAlive(true)
		localConn.(*net.TCPConn).SetKeepAlivePeriod(TcpKeepAlivePeriod)
		if err != nil {
			log.Println(err)
			return
		}
		defer localConn.Close()
		conn.Write([]byte{NEWCONN})
		conn.Write(sp)
		go io.Copy(conn, localConn)
		io.Copy(localConn, conn)
	}
	for isContinue {
		func() {
			defer Recover()
			defer time.Sleep(RetryTime)
			log.Println("Connecting to server...")
			serverConn, err := net.Dial("tcp", config.Server)
			if err != nil {
				return
			}
			defer serverConn.Close()
			log.Println("Successfully connected to server")
			clinfo, err := json.Marshal(config)
			// 发送客户端信息
			// START info_len info
			serverConn.Write([]byte{START})
			var ilen = uint64(len(clinfo))
			serverConn.Write([]byte{
				uint8(ilen >> 56),
				uint8((ilen >> 48) & 0xff),
				uint8((ilen >> 40) & 0xff),
				uint8((ilen >> 32) & 0xff),
				uint8((ilen >> 24) & 0xff),
				uint8((ilen >> 16) & 0xff),
				uint8((ilen >> 8) & 0xff),
				uint8(ilen & 0xff),
			})
			serverConn.Write(clinfo)
			// 读取返回信息
			// SUCCESS / ERROR
			var recvcmd = make([]byte, 1)
			io.ReadAtLeast(serverConn, recvcmd, 1)
			if recvcmd[0] != SUCCESS {
				// 密码错误
				log.Println("Wrong password")
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
					// 读取远端端口
					sp := make([]byte, 2)
					io.ReadAtLeast(serverConn, sp, 2)
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
	signal.Notify(psignal, syscall.SIGINT, syscall.SIGKILL)
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
