package graphsyncimpl

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-graphsync"
	"github.com/ipld/go-ipld-prime"
	cidlink "github.com/ipld/go-ipld-prime/linking/cid"
	"github.com/libp2p/go-libp2p-core/host"
	"github.com/libp2p/go-libp2p-core/peer"

	datatransfer "github.com/filecoin-project/go-data-transfer"
	"github.com/filecoin-project/go-data-transfer/channels"
	"github.com/filecoin-project/go-data-transfer/message"
	"github.com/filecoin-project/go-data-transfer/network"
	"github.com/filecoin-project/go-storedcounter"
	"github.com/hannahhoward/go-pubsub"
)

// This file implements a VERY simple, incomplete version of the data transfer
// module that allows us to make the necessary insertions of data transfer
// functionality into the storage market
// It does not:
// -- support multiple subscribers
// -- do any actual network coordination or use Graphsync

type validateType struct {
	voucherType reflect.Type                  // nolint: structcheck
	validator   datatransfer.RequestValidator // nolint: structcheck
}

type graphsyncImpl struct {
	dataTransferNetwork network.DataTransferNetwork
	validatedTypes      map[string]validateType
	pubSub              *pubsub.PubSub
	channels            *channels.Channels
	gs                  graphsync.GraphExchange
	peerID              peer.ID
	storedCounter       *storedcounter.StoredCounter
}

type internalEvent struct {
	evt   datatransfer.Event
	state datatransfer.ChannelState
}

func dispatcher(evt pubsub.Event, subscriberFn pubsub.SubscriberFn) error {
	ie, ok := evt.(internalEvent)
	if !ok {
		return errors.New("wrong type of event")
	}
	cb, ok := subscriberFn.(datatransfer.Subscriber)
	if !ok {
		return errors.New("wrong type of event")
	}
	cb(ie.evt, ie.state)
	return nil
}

// NewGraphSyncDataTransfer initializes a new graphsync based data transfer manager
func NewGraphSyncDataTransfer(host host.Host, gs graphsync.GraphExchange, storedCounter *storedcounter.StoredCounter) datatransfer.Manager {
	dataTransferNetwork := network.NewFromLibp2pHost(host)
	impl := &graphsyncImpl{
		dataTransferNetwork,
		make(map[string]validateType),
		pubsub.New(dispatcher),
		channels.New(),
		gs,
		host.ID(),
		storedCounter,
	}
	gs.RegisterIncomingRequestHook(impl.gsReqRecdHook)
	gs.RegisterCompletedResponseListener(impl.gsCompletedResponseListener)
	dtReceiver := &graphsyncReceiver{impl}
	dataTransferNetwork.SetDelegate(dtReceiver)
	return impl
}

// gsReqRecdHook is a graphsync.OnRequestReceivedHook hook
// if an incoming request does not match a previous push request, it returns an error.
func (impl *graphsyncImpl) gsReqRecdHook(p peer.ID, request graphsync.RequestData, hookActions graphsync.IncomingRequestHookActions) {

	// if this is a push request the sender is us.
	transferData, err := getExtensionData(request)
	if err != nil {
		hookActions.TerminateWithError(err)
		return
	}

	raw, _ := request.Extension(ExtensionDataTransfer)
	respData := graphsync.ExtensionData{Name: ExtensionDataTransfer, Data: raw}

	// extension not found; probably not our request.
	if transferData == nil {
		return
	}

	sender := impl.peerID
	chid := transferData.GetChannelID()

	if impl.channels.GetByIDAndSender(chid, sender) == datatransfer.EmptyChannelState {
		hookActions.TerminateWithError(err)
		return
	}

	hookActions.ValidateRequest()
	hookActions.SendExtensionData(respData)
}

// gsCompletedResponseListener is a graphsync.OnCompletedResponseListener. We use it learn when the data transfer is complete
// for the side that is responding to a graphsync request
func (impl *graphsyncImpl) gsCompletedResponseListener(p peer.ID, request graphsync.RequestData, status graphsync.ResponseStatusCode) {
	transferData, err := getExtensionData(request)
	if err != nil || transferData == nil {
		return
	}

	sender := impl.peerID
	chid := transferData.GetChannelID()

	chst := impl.channels.GetByIDAndSender(chid, sender)
	if chst == datatransfer.EmptyChannelState {
		return
	}

	evt := datatransfer.Event{
		Code:      datatransfer.Error,
		Timestamp: time.Now(),
	}
	if status == graphsync.RequestCompletedFull {
		evt.Code = datatransfer.Complete
	}
	impl.pubSub.Publish(internalEvent{evt, chst})
}

// RegisterVoucherType registers a validator for the given voucher type
// returns error if:
// * voucher type does not implement voucher
// * there is a voucher type registered with an identical identifier
// * voucherType's Kind is not reflect.Ptr
func (impl *graphsyncImpl) RegisterVoucherType(voucherType reflect.Type, validator datatransfer.RequestValidator) error {
	if voucherType.Kind() != reflect.Ptr {
		return fmt.Errorf("voucherType must be a reflect.Ptr Kind")
	}
	v := reflect.New(voucherType.Elem())
	voucher, ok := v.Interface().(datatransfer.Voucher)
	if !ok {
		return fmt.Errorf("voucher does not implement Voucher interface")
	}

	_, isReg := impl.validatedTypes[voucher.Type()]
	if isReg {
		return fmt.Errorf("voucher type already registered: %s", voucherType.String())
	}

	impl.validatedTypes[voucher.Type()] = validateType{
		voucherType: voucherType,
		validator:   validator,
	}
	return nil
}

// OpenPushDataChannel opens a data transfer that will send data to the recipient peer and
// transfer parts of the piece that match the selector
func (impl *graphsyncImpl) OpenPushDataChannel(ctx context.Context, requestTo peer.ID, voucher datatransfer.Voucher, baseCid cid.Cid, selector ipld.Node) (datatransfer.ChannelID, error) {
	tid, err := impl.sendDtRequest(ctx, selector, false, voucher, baseCid, requestTo)
	if err != nil {
		return datatransfer.ChannelID{}, err
	}

	chid, err := impl.channels.CreateNew(tid, baseCid, selector, voucher,
		impl.peerID, impl.peerID, requestTo) // initiator = us, sender = us, receiver = them
	if err != nil {
		return chid, err
	}
	return chid, nil
}

// OpenPullDataChannel opens a data transfer that will request data from the sending peer and
// transfer parts of the piece that match the selector
func (impl *graphsyncImpl) OpenPullDataChannel(ctx context.Context, requestTo peer.ID, voucher datatransfer.Voucher, baseCid cid.Cid, selector ipld.Node) (datatransfer.ChannelID, error) {

	tid, err := impl.sendDtRequest(ctx, selector, true, voucher, baseCid, requestTo)
	if err != nil {
		return datatransfer.ChannelID{}, err
	}
	// initiator = us, sender = them, receiver = us
	chid, err := impl.channels.CreateNew(tid, baseCid, selector, voucher,
		impl.peerID, requestTo, impl.peerID)
	if err != nil {
		return chid, err
	}
	return chid, nil
}

// sendDtRequest encapsulates message creation and posting to the data transfer network with the provided parameters
func (impl *graphsyncImpl) sendDtRequest(ctx context.Context, selector ipld.Node, isPull bool, voucher datatransfer.Voucher, baseCid cid.Cid, to peer.ID) (datatransfer.TransferID, error) {
	sbytes, err := nodeAsBytes(selector)
	if err != nil {
		return 0, err
	}
	vbytes, err := voucher.ToBytes()
	if err != nil {
		return 0, err
	}
	next, err := impl.storedCounter.Next()
	if err != nil {
		return 0, err
	}
	tid := datatransfer.TransferID(next)
	req := message.NewRequest(tid, isPull, voucher.Type(), vbytes, baseCid, sbytes)

	if err := impl.dataTransferNetwork.SendMessage(ctx, to, req); err != nil {
		return 0, err
	}
	return tid, nil
}

func (impl *graphsyncImpl) sendResponse(ctx context.Context, isAccepted bool, to peer.ID, tid datatransfer.TransferID) {
	resp := message.NewResponse(tid, isAccepted)
	if err := impl.dataTransferNetwork.SendMessage(ctx, to, resp); err != nil {
		log.Error(err)
	}
}

// close an open channel (effectively a cancel)
func (impl *graphsyncImpl) CloseDataTransferChannel(x datatransfer.ChannelID) {}

// get status of a transfer
func (impl *graphsyncImpl) TransferChannelStatus(x datatransfer.ChannelID) datatransfer.Status {
	return datatransfer.ChannelNotFoundError
}

// get notified when certain types of events happen
func (impl *graphsyncImpl) SubscribeToEvents(subscriber datatransfer.Subscriber) datatransfer.Unsubscribe {
	return datatransfer.Unsubscribe(impl.pubSub.Subscribe(subscriber))
}

// get all in progress transfers
func (impl *graphsyncImpl) InProgressChannels() map[datatransfer.ChannelID]datatransfer.ChannelState {
	return impl.channels.InProgress()
}

// sendGsRequest assembles a graphsync request and determines if the transfer was completed/successful.
// notifies subscribers of final request status.
func (impl *graphsyncImpl) sendGsRequest(ctx context.Context, initiator peer.ID, transferID datatransfer.TransferID, isPull bool, dataSender peer.ID, root cidlink.Link, stor ipld.Node) {
	extDtData := newTransferData(transferID, initiator, isPull)
	var buf bytes.Buffer
	if err := extDtData.MarshalCBOR(&buf); err != nil {
		log.Error(err)
	}
	extData := buf.Bytes()
	_, errChan := impl.gs.Request(ctx, dataSender, root, stor,
		graphsync.ExtensionData{
			Name: ExtensionDataTransfer,
			Data: extData,
		})
	go func() {
		var lastError error
		for err := range errChan {
			lastError = err
		}
		evt := datatransfer.Event{
			Code:      datatransfer.Error,
			Timestamp: time.Now(),
		}
		chid := datatransfer.ChannelID{Initiator: initiator, ID: transferID}
		chst := impl.channels.GetByIDAndSender(chid, dataSender)
		if chst == datatransfer.EmptyChannelState {
			msg := "cannot find a matching channel for this request"
			evt.Message = msg
		} else {
			if lastError == nil {
				evt.Code = datatransfer.Complete
			} else {
				evt.Message = lastError.Error()
			}
		}
		impl.pubSub.Publish(internalEvent{evt, chst})
	}()
}
