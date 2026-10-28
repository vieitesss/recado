package harness

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"time"
)

// ack is the one line a shim writes back after recado pushed a frame: whether
// the harness accepted the message as a turn, and why not when it did not.
type ack struct {
	OK    *bool  `json:"ok"`
	Error string `json:"error"`
}

// awaitAck reads the shim's answer to one pushed frame. A shim answers only
// after its harness took the message (or refused it), so a missing answer
// within timeout means recado cannot tell whether it arrived: the caller
// releases the message to stored, and a later drain retries it, at worst
// delivering it twice.
func awaitAck(conn net.Conn, timeout time.Duration, name string) error {
	if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return fmt.Errorf("%s: the shim did not acknowledge the message: %w", name, err)
	}
	var a ack
	if err := json.Unmarshal([]byte(line), &a); err != nil || a.OK == nil {
		return fmt.Errorf("%s: the shim answered %q, not an acknowledgement", name, strings.TrimSpace(line))
	}
	if !*a.OK {
		reason := a.Error
		if reason == "" {
			reason = "the harness refused it"
		}
		return fmt.Errorf("%s: %s", name, reason)
	}
	return nil
}
