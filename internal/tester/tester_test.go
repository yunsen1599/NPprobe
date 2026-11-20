package tester

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/yunsen1599/NPprobe/internal/panel"
)

type fakeProxy struct {
	fails int // 前 fails 次失败
	calls int
}

func (f *fakeProxy) URLTest(context.Context, string, []uint16) (uint16, error) {
	f.calls++
	if f.calls <= f.fails {
		return 0, errors.New("timeout")
	}
	return 123, nil
}

func okDial(context.Context, string, string) (net.Conn, error) {
	a, b := net.Pipe()
	_ = b.Close()
	return a, nil
}

func newTester() *Tester {
	return &Tester{URLs: []string{"u1", "u2"}, Timeout: time.Second, Dial: okDial, Sleep: func(time.Duration) {}}
}

func TestOK(t *testing.T) {
	r := newTester().Test(context.Background(), panel.Target{NodeID: 1, Protocol: "vless", Address: "a", Port: 443}, &fakeProxy{})
	if r.Status != StatusOK || r.DelayMS != 123 || r.NodeID != 1 {
		t.Fatalf("%+v", r)
	}
}

func TestFallbackURLAndRetry(t *testing.T) {
	// u1、u2 都失败（2 次），重试后 u1 成功
	p := &fakeProxy{fails: 2}
	r := newTester().Test(context.Background(), panel.Target{NodeID: 1, Protocol: "vless", Address: "a", Port: 443}, p)
	if r.Status != StatusOK || p.calls != 3 {
		t.Fatalf("%+v calls=%d", r, p.calls)
	}
	p = &fakeProxy{fails: 100}
	r = newTester().Test(context.Background(), panel.Target{NodeID: 1, Protocol: "vless", Address: "a", Port: 443}, p)
	if r.Status != StatusProxyFail || p.calls != 4 || r.Error != "timeout" {
		t.Fatalf("%+v calls=%d", r, p.calls)
	}
}

func TestTCPFailAndUDPSkip(t *testing.T) {
	tt := newTester()
	tt.Dial = func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("refused") }
	p := &fakeProxy{}
	r := tt.Test(context.Background(), panel.Target{NodeID: 1, Protocol: "trojan", Address: "a", Port: 443}, p)
	if r.Status != StatusTCPFail || p.calls != 0 {
		t.Fatalf("tcp fail must short-circuit: %+v", r)
	}
	r = tt.Test(context.Background(), panel.Target{NodeID: 1, Protocol: "hysteria2", Address: "a", Port: 443}, p)
	if r.Status != StatusOK {
		t.Fatalf("udp protocols skip the tcp step: %+v", r)
	}
}

func TestSpeedHelpers(t *testing.T) {
	if Kbps(1_000_000, time.Second) != 8000 || Kbps(1000, time.Second) != 0 {
		t.Fatal("Kbps")
	}
	hits := 0
	for round := int64(0); round < 12; round++ {
		if SpeedDue(5, round, 12) {
			hits++
		}
	}
	if hits != 1 || SpeedDue(5, 0, 0) {
		t.Fatalf("each node once per 12 rounds, got %d", hits)
	}
}
