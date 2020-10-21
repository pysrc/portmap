package main

import (
	"bytes"
	"crypto/md5"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// ServerConfig 服务端配置
type ServerConfig struct {
	Key  string   `json:"key"`
	Port uint16   `json:"port"`
	Open []uint16 `json:"open"`
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
	// IdleTime 心跳检测时间
	IdleTime = time.Second * 10
)

// KeyMd5 获取key的md5
func KeyMd5(key string) []byte {
	d5 := md5.New()
	d5.Write([]byte(key))
	return d5.Sum(nil)
}

// DoServer 服务端处理
func DoServer(config *ServerConfig) {
	if config == nil {
		return
	}
	clientMap := make(map[uint16]net.Conn)
	chanMap := make(map[uint16]chan net.Conn)
	// 守护客户端连接
	go func() {
		rt := make([]byte, 1)
		for {
			time.Sleep(IdleTime)
			for p, c := range clientMap {
				_, err := c.Write([]byte{IDLE})
				if err != nil {
					log.Println(err)
					delete(clientMap, p)
					if c != nil {
						c.Close()
					}
					log.Println("Delete connect", p)
					continue
				}
				_, err = io.ReadAtLeast(c, rt, 1)
				if err != nil || rt[0] != SUCCESS {
					log.Println(err)
					delete(clientMap, p)
					if c != nil {
						c.Close()
					}
					log.Println("Delete connect", p)
					continue
				}
			}

		}
	}()
	// 监听映射端口
	var dolisten = func(port uint16) {
		chanMap[port] = make(chan net.Conn)
		liscon, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%v", port))
		if err != nil {
			panic(err)
		}
		defer liscon.Close()
		for {
			conn, err := liscon.Accept()
			if err != nil {
				log.Println(err)
				continue
			}
			client := clientMap[port]
			if client != nil {
				// 新连接，发送命令
				client.Write([]byte{NEWSOCKET})
				client.Write([]byte{uint8(port >> 8), uint8(port & 0xff)})
				chanMap[port] <- conn
			} else {
				conn.Close()
			}
		}
	}
	// 监听所有映射端口
	for _, p := range config.Open {
		go dolisten(p)
	}
	lis, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%v", config.Port))
	if err != nil {
		panic(err)
	}
	defer lis.Close()
	var doconn = func(conn net.Conn) {
		// defer conn.Close()
		var cmd = make([]byte, 1)
		io.ReadAtLeast(conn, cmd, 1)
		switch cmd[0] {
		case START:
			// 初始化
			// 读取keymd5
			rkeymd5 := make([]byte, 16)
			io.ReadAtLeast(conn, rkeymd5, 16)
			if !bytes.Equal(rkeymd5, KeyMd5(config.Key)) {
				conn.Write([]byte{ERROR})
				msg := []byte("Key认证失败！")
				ml := uint16(len(msg))
				conn.Write([]byte{uint8(ml >> 8), uint8(ml & 0xff)})
				conn.Write(msg)
				conn.Close()
				return
			}
			// 读取端口
			for {
				portBuf := make([]byte, 2)
				io.ReadAtLeast(conn, portBuf, 2)
				prt := uint16(portBuf[0])<<8 + uint16(portBuf[1])
				if prt == 0 {
					break
				}
				if _, ok := clientMap[prt]; ok {
					// 端口被用了
					conn.Write([]byte{ERROR})
					msg := []byte(fmt.Sprintf("端口(%v)已经被使用！", prt))
					ml := uint16(len(msg))
					conn.Write([]byte{uint8(ml >> 8), uint8(ml & 0xff)})
					conn.Write(msg)
					conn.Close()
					return
				}
				clientMap[prt] = conn
				log.Println("Open port", prt)
			}
			conn.Write([]byte{SUCCESS})
		case NEWCONN:
			// 客户端新建立连接
			sport := make([]byte, 2)
			io.ReadAtLeast(conn, sport, 2)
			pt := (uint16(sport[0]) << 8) + uint16(sport[1])
			client := clientMap[pt]
			if client != nil {
				if rconn, ok := <-chanMap[pt]; ok {
					go io.Copy(rconn, conn)
					io.Copy(conn, rconn)
					conn.Close()
					rconn.Close()
				}
			} else {
				conn.Close()
			}
		case KILL:
			// 退出
			rkeymd5 := make([]byte, 16)
			io.ReadAtLeast(conn, rkeymd5, 16)
			if bytes.Equal(rkeymd5, KeyMd5(config.Key)) {
				log.Println("Exit !")
				conn.Write([]byte{SUCCESS})
				conn.Close()
				os.Exit(0)
			} else {
				conn.Write([]byte{ERROR})
				msg := []byte("Key认证失败！")
				ml := uint16(len(msg))
				conn.Write([]byte{uint8(ml >> 8), uint8(ml & 0xff)})
				conn.Write(msg)
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
	serverConn, err := net.Dial("tcp", config.Server)
	if err != nil {
		panic(err)
	}
	defer serverConn.Close()
	var portmap = make(map[uint16]string, len(config.Map))
	for _, m := range config.Map {
		portmap[m.Outer] = m.Inner
	}
	// 发送key
	serverConn.Write([]byte{START})
	serverConn.Write(KeyMd5(config.Key))
	// 发送端口信息
	for p := range portmap {
		serverConn.Write([]byte{uint8(p >> 8), uint8(p & 0xff)})
	}
	serverConn.Write([]byte{0, 0})
	// 新建连接处理
	var doconn = func(conn net.Conn, sport uint16, sp []byte) {
		defer conn.Close()
		localConn, err := net.Dial("tcp", portmap[sport])
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
	// 进入指令读取循环
	var cmd = make([]byte, 1)
	for {
		serverConn.Read(cmd)
		switch cmd[0] {
		case ERROR:
			// 处理出错
			msglen := make([]byte, 2)
			io.ReadAtLeast(serverConn, msglen, 2)
			ml := int((uint16(msglen[0]) << 8) + uint16(msglen[1]))
			msg := make([]byte, ml)
			io.ReadAtLeast(serverConn, msg, ml)
			log.Println(string(msg))
			return
		case NEWSOCKET:
			// 新建连接
			// 读取远端端口
			sp := make([]byte, 2)
			io.ReadAtLeast(serverConn, sp, 2)
			sport := uint16(sp[0])<<8 + uint16(sp[1])
			conn, err := net.Dial("tcp", config.Server)
			if err != nil {
				panic(err)
			}
			go doconn(conn, sport, sp)
		case IDLE:
			serverConn.Write([]byte{SUCCESS})
		}
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
