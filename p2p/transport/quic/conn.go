package libp2pquic

import (
	"context"

	ic "github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	tpt "github.com/libp2p/go-libp2p/core/transport"

	ma "github.com/multiformats/go-multiaddr"
	"github.com/quic-go/quic-go"
)

type conn struct {
	quicConn  *quic.Conn
	transport *transport
	scope     network.ConnManagementScope

	localPeer      peer.ID
	localMultiaddr ma.Multiaddr

	remotePeerID    peer.ID
	remotePubKey    ic.PubKey
	remoteMultiaddr ma.Multiaddr
}

var _ tpt.CapableConn = &conn{}

// Close closes the connection.
// It must be called even if the peer closed the connection in order for
// garbage collection to properly work in this package.
func (c *conn) Close() error {
	return c.closeWithError(0, "")
}

// CloseWithError closes the connection
// It must be called even if the peer closed the connection in order for
// garbage collection to properly work in this package.
func (c *conn) CloseWithError(errCode network.ConnErrorCode) error {
	return c.closeWithError(quic.ApplicationErrorCode(errCode), "")
}

func (c *conn) closeWithError(errCode quic.ApplicationErrorCode, errString string) error {
	c.transport.removeConn(c.quicConn)
	err := c.quicConn.CloseWithError(errCode, errString)
	c.scope.Done()
	return err
}

// IsClosed returns whether a connection is fully closed.
func (c *conn) IsClosed() bool {
	return c.quicConn.Context().Err() != nil
}

func (c *conn) allowWindowIncrease(size uint64) bool {
	return c.scope.ReserveMemory(int(size), network.ReservationPriorityMedium) == nil
}

// OpenStream creates a new stream.
func (c *conn) OpenStream(ctx context.Context) (network.MuxedStream, error) {
	qstr, err := c.quicConn.OpenStreamSync(ctx)
	if err != nil {
		return nil, parseStreamError(err)
	}
	return &stream{Stream: qstr}, nil
}

// AcceptStream accepts a stream opened by the other side.
func (c *conn) AcceptStream() (network.MuxedStream, error) {
	qstr, err := c.quicConn.AcceptStream(context.Background())
	if err != nil {
		return nil, parseStreamError(err)
	}
	return &stream{Stream: qstr}, nil
}

// LocalPeer returns our peer ID
func (c *conn) LocalPeer() peer.ID { return c.localPeer }

// RemotePeer returns the peer ID of the remote peer.
func (c *conn) RemotePeer() peer.ID { return c.remotePeerID }

// RemotePublicKey returns the public key of the remote peer.
func (c *conn) RemotePublicKey() ic.PubKey { return c.remotePubKey }

// LocalMultiaddr returns the local Multiaddr associated
func (c *conn) LocalMultiaddr() ma.Multiaddr { return c.localMultiaddr }

// RemoteMultiaddr returns the remote Multiaddr associated
func (c *conn) RemoteMultiaddr() ma.Multiaddr { return c.remoteMultiaddr }

func (c *conn) Transport() tpt.Transport { return c.transport }

func (c *conn) Scope() network.ConnScope { return c.scope }

// ConnState is the state of security connection.
func (c *conn) ConnState() network.ConnectionState {
	t := "quic-v1"
	if _, err := c.LocalMultiaddr().ValueForProtocol(ma.P_QUIC); err == nil {
		t = "quic"
	}
	return network.ConnectionState{Transport: t}
}
