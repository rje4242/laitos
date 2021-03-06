package common

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"net"
	"testing"
	"time"

	"github.com/HouzuoGuo/laitos/lalog"
	"github.com/HouzuoGuo/laitos/misc"
)

type TCPTestApp struct {
	stats *misc.Stats
}

func (app *TCPTestApp) GetTCPStatsCollector() *misc.Stats {
	return app.stats
}

func (app *TCPTestApp) HandleTCPConnection(logger lalog.Logger, clientIP string, conn *net.TCPConn) {
	if clientIP == "" {
		panic("client IP must not be empty")
	}
	if n, err := conn.Write([]byte("hello")); err != nil || n != 5 {
		log.Panicf("n %d err %v", n, err)
	}
}

func TestTCPServer(t *testing.T) {
	srv := TCPServer{
		ListenAddr:  "127.0.0.1",
		ListenPort:  62172,
		AppName:     "TestTCPServer",
		App:         &TCPTestApp{stats: misc.NewStats()},
		LimitPerSec: 5,
	}
	srv.Initialise()

	// Expect server to start within three seconds
	var shutdown bool
	go func() {
		if err := srv.StartAndBlock(); err != nil {
			panic(err)
		}
		shutdown = true
	}()
	time.Sleep(3 * time.Second)

	// Connect to the server and expect a hello response
	client, err := net.Dial("tcp", fmt.Sprintf("%s:%d", srv.ListenAddr, srv.ListenPort))
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(client)
	str, err := reader.ReadString(0)
	if err != io.EOF {
		t.Fatal(err)
	}
	if str != "hello" {
		t.Fatal(str)
	}

	// Wait for connection to close and then check stats counter
	time.Sleep(ServerRateLimitIntervalSec * 2)
	if count := srv.App.GetTCPStatsCollector().Count(); count != 1 {
		t.Fatal(count)
	}

	// Attempt to exceed the rate limit via connection attempts
	var success int
	for i := 0; i < 10; i++ {
		client, err := net.Dial("tcp", fmt.Sprintf("%s:%d", srv.ListenAddr, srv.ListenPort))
		if err != nil {
			t.Fatal(err)
		}
		reader := bufio.NewReader(client)
		str, _ := reader.ReadString(0)
		if str == "hello" {
			success++
		}
		time.Sleep(100 * time.Millisecond)
	}
	if success > srv.LimitPerSec*2 || success < srv.LimitPerSec/2 {
		t.Fatal(success)
	}

	// Attempt to exceed the rate limit via conversation
	time.Sleep(ServerRateLimitIntervalSec * 2)
	success = 0
	for i := 0; i < 10; i++ {
		if srv.AddAndCheckRateLimit("test") {
			success++
		}
	}
	if success > srv.LimitPerSec*2 || success < srv.LimitPerSec/2 {
		t.Fatal(success)
	}

	// Server must shut down within three seconds
	srv.Stop()
	time.Sleep(3 * time.Second)
	if !shutdown {
		t.Fatal("did not shut down")
	}

	// It is OK to repeatedly shut down a server
	srv.Stop()
	srv.Stop()
}
