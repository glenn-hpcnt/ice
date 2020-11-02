package ice

import (
	"context"
	"fmt"
	"net"
	"sync/atomic"
	"time"

	"github.com/pion/logging"
	"github.com/pion/stun"
)

type candidateBase struct {
	id            string
	networkType   NetworkType
	candidateType CandidateType

	component      uint16
	address        string
	port           int
	relatedAddress *CandidateRelatedAddress
	tcpType        TCPType

	resolvedAddr net.Addr

	lastSent     atomic.Value
	lastReceived atomic.Value
	conn         net.PacketConn

	currAgent *Agent
	closeCh   chan struct{}
	closedCh  chan struct{}
}

// Done implements context.Context
func (c *candidateBase) Done() <-chan struct{} {
	return c.closeCh
}

// Err implements context.Context
func (c *candidateBase) Err() error {
	select {
	case <-c.closedCh:
		return ErrRunCanceled
	default:
		return nil
	}
}

// Deadline implements context.Context
func (c *candidateBase) Deadline() (deadline time.Time, ok bool) {
	return time.Time{}, false
}

// Value implements context.Context
func (c *candidateBase) Value(key interface{}) interface{} {
	return nil
}

// ID returns Candidate ID
func (c *candidateBase) ID() string {
	return c.id
}

// Address returns Candidate Address
func (c *candidateBase) Address() string {
	return c.address
}

// Port returns Candidate Port
func (c *candidateBase) Port() int {
	return c.port
}

// Type returns candidate type
func (c *candidateBase) Type() CandidateType {
	return c.candidateType
}

// NetworkType returns candidate NetworkType
func (c *candidateBase) NetworkType() NetworkType {
	return c.networkType
}

// Component returns candidate component
func (c *candidateBase) Component() uint16 {
	return c.component
}

// LocalPreference returns the local preference for this candidate
func (c *candidateBase) LocalPreference() uint16 {
	return defaultLocalPreference
}

// RelatedAddress returns *CandidateRelatedAddress
func (c *candidateBase) RelatedAddress() *CandidateRelatedAddress {
	return c.relatedAddress
}

func (c *candidateBase) TCPType() TCPType {
	return c.tcpType
}

// start runs the candidate using the provided connection
func (c *candidateBase) start(a *Agent, conn net.PacketConn, initializedCh <-chan struct{}) {
	if c.conn != nil {
		c.agent().log.Warn("Can't start already started candidateBase")
		return
	}
	c.currAgent = a
	c.conn = conn
	c.closeCh = make(chan struct{})
	c.closedCh = make(chan struct{})

	go c.recvLoop(initializedCh)
}

func (c *candidateBase) recvLoop(initializedCh <-chan struct{}) {
	defer func() {
		close(c.closedCh)
		c.agent().log.Errorf("glenn recvloop defer called %s", c.id)
	}()
	c.agent().log.Errorf("glenn recvloop start %s %s", c.id, c.conn)
	select {
	case <-initializedCh:
		c.agent().log.Errorf("glenn recvloop initializedCh %s %s", c.id, c.conn)
	case <-c.closeCh:
		c.agent().log.Errorf("glenn recvloop closeCh %s %s", c.id, c.conn)
		return
	}

	log := c.agent().log
	buffer := make([]byte, receiveMTU)
	for {
		n, srcAddr, err := c.conn.ReadFrom(buffer)
		if err != nil {
			c.agent().log.Errorf("glenn recvloop connection closed %s %s", c.id, c.conn)
			return
		}
		handleInboundCandidateMsg(c, c, buffer[:n], srcAddr, log)
	}
}

func handleInboundCandidateMsg(ctx context.Context, c Candidate, buffer []byte, srcAddr net.Addr, log logging.LeveledLogger) {
	defer c.agent().log.Errorf("glenn handleInboundCandidateMsg end %s", c.ID())
	c.agent().log.Errorf("glenn handleInboundCandidateMsg start %s", c.ID())
	if stun.IsMessage(buffer) {
		m := &stun.Message{
			Raw: make([]byte, len(buffer)),
		}
		// Explicitly copy raw buffer so Message can own the memory.
		copy(m.Raw, buffer)
		if err := m.Decode(); err != nil {
			log.Warnf("Failed to handle decode ICE from %s to %s: %v", c.addr(), srcAddr, err)
			return
		}
		err := c.agent().run(ctx, func(ctx context.Context, agent *Agent) {
			agent.handleInbound(m, c, srcAddr)
		})
		if err != nil {
			log.Warnf("Failed to handle message: %v", err)
		}

		return
	}

	if !c.agent().validateNonSTUNTraffic(c, srcAddr) {
		log.Warnf("Discarded message from %s, not a valid remote candidate", c.addr())
		return
	}

	// NOTE This will return packetio.ErrFull if the buffer ever manages to fill up.
	if _, err := c.agent().buffer.Write(buffer); err != nil {
		log.Warnf("failed to write packet")
	}
}

// close stops the recvLoop
func (c *candidateBase) close() error {
	// If conn has never been started will be nil
	if c.Done() == nil {
		return nil
	}

	// Assert that conn has not already been closed
	select {
	case <-c.Done():
		return nil
	default:
	}

	var firstErr error

	// Unblock recvLoop
	close(c.closeCh)
	if err := c.conn.SetDeadline(time.Now()); err != nil {
		firstErr = err
	}

	// Close the conn
	c.agent().log.Errorf("glenn c.conn.Close() %s, %s", c.id, c.conn)
	if err := c.conn.Close(); err != nil && firstErr == nil {
		firstErr = err
	}

	if firstErr != nil {
		return firstErr
	}

	// Wait until the recvLoop is closed
	c.agent().log.Errorf("glenn closedCh start %s, %s", c.id, c.conn)
	<-c.closedCh
	c.agent().log.Errorf("glenn closedCh end %s", c.id)
	return nil
}

func (c *candidateBase) writeTo(raw []byte, dst Candidate) (int, error) {
	n, err := c.conn.WriteTo(raw, dst.addr())
	if err != nil {
		return n, fmt.Errorf("failed to send packet: %v", err)
	}
	c.seen(true)
	return n, nil
}

// Priority computes the priority for this ICE Candidate
func (c *candidateBase) Priority() uint32 {
	// The local preference MUST be an integer from 0 (lowest preference) to
	// 65535 (highest preference) inclusive.  When there is only a single IP
	// address, this value SHOULD be set to 65535.  If there are multiple
	// candidates for a particular component for a particular data stream
	// that have the same type, the local preference MUST be unique for each
	// one.
	return (1<<24)*uint32(c.Type().Preference()) +
		(1<<8)*uint32(c.LocalPreference()) +
		uint32(256-c.Component())
}

// Equal is used to compare two candidateBases
func (c *candidateBase) Equal(other Candidate) bool {
	return c.NetworkType() == other.NetworkType() &&
		c.Type() == other.Type() &&
		c.Address() == other.Address() &&
		c.Port() == other.Port() &&
		c.RelatedAddress().Equal(other.RelatedAddress())
}

// String makes the candidateBase printable
func (c *candidateBase) String() string {
	return fmt.Sprintf("%s %s %s:%d%s", c.NetworkType(), c.Type(), c.Address(), c.Port(), c.relatedAddress)
}

// LastReceived returns a time.Time indicating the last time
// this candidate was received
func (c *candidateBase) LastReceived() time.Time {
	lastReceived := c.lastReceived.Load()
	if lastReceived == nil {
		return time.Time{}
	}
	return lastReceived.(time.Time)
}

func (c *candidateBase) setLastReceived(t time.Time) {
	c.lastReceived.Store(t)
}

// LastSent returns a time.Time indicating the last time
// this candidate was sent
func (c *candidateBase) LastSent() time.Time {
	lastSent := c.lastSent.Load()
	if lastSent == nil {
		return time.Time{}
	}
	return lastSent.(time.Time)
}

func (c *candidateBase) setLastSent(t time.Time) {
	c.lastSent.Store(t)
}

func (c *candidateBase) seen(outbound bool) {
	if outbound {
		c.setLastSent(time.Now())
	} else {
		c.setLastReceived(time.Now())
	}
}

func (c *candidateBase) addr() net.Addr {
	return c.resolvedAddr
}

func (c *candidateBase) agent() *Agent {
	return c.currAgent
}

func (c *candidateBase) context() context.Context {
	return c
}
