package vm

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestParseTCP(t *testing.T) {
	const tcp = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:A3F1 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 1 1 0000000000000000 100 0 0 10 0
   1: 0100007F:A3F1 0100007F:D2B4 01 00000000:00000000 00:00000000 00000000  1000        0 2 1 0000000000000000 20 4 30 10 -1
   2: 0100007F:D2B4 0100007F:A3F1 01 00000000:00000000 00:00000000 00000000  1000        0 3 1 0000000000000000 20 4 30 10 -1
   3: 0F02000A:0016 0202000A:9C40 01 00000000:00000000 00:00000000 00000000  1000        0 4 1 0000000000000000 20 4 30 10 -1
   4: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 5 1 0000000000000000 100 0 0 10 0
`
	got, err := parseTCP(strings.NewReader(tcp))
	if err != nil {
		t.Fatal(err)
	}
	// 0xA3F1 = 41969 (a VM's ssh port, connected), 0xD2B4 the client's side;
	// 8080 only listens and 22 isn't on loopback.
	if !got[41969] || got[8080] || got[22] || len(got) != 2 {
		t.Errorf("established = %v", got)
	}
}

func TestParseStatTicks(t *testing.T) {
	stat := "4242 (qemu-system-x86 (x)) S 1 4242 4242 0 -1 4194560 100 0 0 0 1500 250 0 0 20 0 9 0 100 0 0"
	if n, err := parseStatTicks(stat); err != nil || n != 1750 {
		t.Errorf("ticks = %d, %v", n, err)
	}
	if _, err := parseStatTicks("garbage"); err == nil {
		t.Error("garbage parsed")
	}
	if n, err := cpuTicks(os.Getpid()); err != nil || n > 1<<40 {
		t.Errorf("own ticks = %d, %v", n, err)
	}
}

func TestIdleObserve(t *testing.T) {
	t0 := time.Now()
	var st idleState
	if d := st.observe(10, 1000, false, t0); d != 0 {
		t.Errorf("first sample idle %s", d)
	}
	// 1% of a core for 10 minutes: idle.
	if d := st.observe(10, 1000+600, false, t0.Add(10*time.Minute)); d != 10*time.Minute {
		t.Errorf("idle %s", d)
	}
	// 10% of a core: active.
	if d := st.observe(10, 1600+600, false, t0.Add(11*time.Minute)); d != 0 {
		t.Errorf("busy CPU idle %s", d)
	}
	// A connection: active.
	if d := st.observe(10, 2200, true, t0.Add(20*time.Minute)); d != 0 {
		t.Errorf("connected idle %s", d)
	}
	if d := st.observe(10, 2200, false, t0.Add(50*time.Minute)); d != 30*time.Minute {
		t.Errorf("idle %s", d)
	}
	// A restarted VM starts over.
	if d := st.observe(11, 50, false, t0.Add(60*time.Minute)); d != 0 {
		t.Errorf("new QEMU idle %s", d)
	}
}

func TestIdleTimeout(t *testing.T) {
	m := testManager(t)
	if d := m.IdleTimeout(&State{}); d != 0 {
		t.Errorf("default %s", d)
	}
	m.Cfg.IdleStop = "30m"
	if d := m.IdleTimeout(&State{}); d != 30*time.Minute {
		t.Errorf("config %s", d)
	}
	if d := m.IdleTimeout(&State{IdleStop: "off"}); d != 0 {
		t.Errorf("template off %s", d)
	}
	if d := m.IdleTimeout(&State{IdleStop: "2h"}); d != 2*time.Hour {
		t.Errorf("template %s", d)
	}
}
