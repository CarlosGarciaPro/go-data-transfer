package network

import (
	"context"

	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/libp2p/go-libp2p-core/protocol"

	datatransfer "github.com/filecoin-project/go-data-transfer"
)

var (
	// ProtocolDataTransfer is the protocol identifier for graphsync messages
	ProtocolDataTransfer protocol.ID = "/fil/datatransfer/1.0.0"
)

// DataTransferNetwork provides network connectivity for GraphSync.
type DataTransferNetwork interface {
	Protect(id peer.ID, tag string)
	Unprotect(id peer.ID, tag string) bool

	// SendMessage sends a GraphSync message to a peer.
	SendMessage(
		context.Context,
		peer.ID,
		datatransfer.Message) error

	// SetDelegate registers the Reciver to handle messages received from the
	// network.
	SetDelegate(Receiver)

	// StopAcceptingRequests stops accepting requests on the protocol
	StopAcceptingRequests()

	// ConnectTo establishes a connection to the given peer
	ConnectTo(context.Context, peer.ID) error

	// ID returns the peer id of this libp2p host
	ID() peer.ID
}

// Receiver is an interface for receiving messages from the GraphSyncNetwork.
type Receiver interface {
	ReceiveRequest(
		ctx context.Context,
		sender peer.ID,
		incoming datatransfer.Request)

	ReceiveResponse(
		ctx context.Context,
		sender peer.ID,
		incoming datatransfer.Response)

	ReceiveError(error)
}
