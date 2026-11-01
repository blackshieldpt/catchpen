package vm

import (
	"io"
	"net"

	"github.com/blackshieldpt/catchpen/internal/termfilter"
)

// DialConsole connects to a running VM's serial console. Like SSHPort, it
// refuses a VM that is stopped or paused by a canary hit. It checks only when
// it dials: a canary hit later doesn't close the connection.
func (m *Manager) DialConsole(name string) (net.Conn, error) {
	if _, err := m.SSHPort(name); err != nil {
		return nil, err
	}
	return net.Dial("unix", m.SerialSocket(name))
}

// PumpConsole copies in to the console, and the console's output through
// termfilter to out, until the guest hangs up or in ends (in's EOF is how a
// caller detaches). Then it closes conn, and waits for a Write to out under
// way to return, so it never writes to out after it returns; a Write that
// never returns blocks it. A Write to conn that the guest never reads blocks
// the input side, so in's EOF is not seen. After the guest hangs up, the copy
// from in keeps running: it takes what in next delivers and drops it, until
// the caller closes in.
func PumpConsole(conn net.Conn, in io.Reader, out io.Writer) {
	shown := make(chan struct{})
	sent := make(chan struct{})
	go func() { _, _ = io.Copy(termfilter.New(out), conn); close(shown) }()
	go func() { _, _ = io.Copy(conn, in); close(sent) }()
	select {
	case <-shown:
	case <-sent:
	}
	conn.Close()
	<-shown // out is not written after PumpConsole returns
}
