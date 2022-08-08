package transportc

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"sync"

	"github.com/pion/datachannel"
	"github.com/pion/webrtc/v3"
)

// Dialer
type Dialer struct {
	SignalMethod SignalMethod
	MaxReadSize  int

	// WebRTC configuration
	settingEngine webrtc.SettingEngine
	configuration webrtc.Configuration

	// WebRTC PeerConnection
	mutex          sync.Mutex // mutex makes peerConnection thread-safe
	peerConnection *webrtc.PeerConnection
}

// Dial connects to a remote peer with SDP-based negotiation.
//
// Internally calls DialContext with context.Background().
//
// The returned connection is backed by a DataChannel created by the caller
// with the SDP role as OFFERER as defined in RFC3264. If SignalMethod is set,
// the Offer/Answer exchange per new PeerConnection will be done automatically.
// Otherwise, it is recommended to call NewPeerConnection and exchange the SDP
// offer/answer manually before dialing.
func (d *Dialer) Dial(label string) (net.Conn, error) {
	return d.DialContext(context.Background(), label)
}

// DialContext connects to a remote peer with SDP-based negotiation
// using the provided context.
//
// The provided Context must be non-nil. If the context expires before
// the connection is complete, an error is returned. Once successfully
// connected, any expiration of the context will not affect the
// connection.
//
// The returned connection is backed by a DataChannel created by the caller
// with the SDP role as OFFERER as defined in RFC3264. If SignalMethod is set,
// the Offer/Answer exchange per new PeerConnection will be done automatically.
// Otherwise, it is recommended to call NewPeerConnection and exchange the SDP
// offer/answer manually before dialing.
func (d *Dialer) DialContext(ctx context.Context, label string) (net.Conn, error) {
	// check if context is done
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	d.mutex.Lock()
	defer d.mutex.Unlock()

	if d.peerConnection == nil {
		err := d.NewPeerConnection(ctx)
		if err != nil {
			return nil, err
		}
	}

	// create new DataChannel
	dataChannel, err := d.peerConnection.CreateDataChannel(label, nil)
	if err != nil {
		if errors.Is(err, webrtc.ErrConnectionClosed) {
			// could be closed, will try with a new PeerConnection
			err := d.NewPeerConnection(ctx)
			if err != nil {
				return nil, err
			}

			// second attempt to create DataChannel
			// if this fails, no more retries
			dataChannel, err = d.peerConnection.CreateDataChannel(label, nil)
			if err != nil {
				return nil, err
			}
		} else {
			return nil, err
		}
	}

	// set event handlers
	var detachChan chan datachannel.ReadWriteCloser = make(chan datachannel.ReadWriteCloser)
	dataChannel.OnOpen(func() {
		// detach from wrapper
		dc, err := dataChannel.Detach()
		if err != nil {
			detachChan <- nil
		} else {
			detachChan <- dc
		}
	})

	dataChannel.OnClose(func() {
		// TODO: possibly tear down the PeerConnection if it is the last DataChannel?
	})

	// OnError won't be used as pion's readLoop is ignored
	// dataChannel.OnError(func(err error) {
	// })

	// wait for datachannel
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case dataChannelDetach := <-detachChan:
		if dataChannelDetach == nil {
			return nil, errors.New("failed to receive datachannel")
		}
		conn := &Conn{
			dataChannel:       dataChannelDetach,
			readMaxPacketSize: d.MaxReadSize,
			readBuf:           make(chan []byte),
		}
		go conn.readLoop() // start the read loop

		return conn, nil
	}
}

// Close closes the WebRTC PeerConnection and with it
// all the WebRTC DataChannels under it.
//
// SHOULD be called when done using the transport.
func (d *Dialer) Close() error {
	return d.peerConnection.Close()
}

// NewPeerConnection creates a new PeerConnection to be used for the next Dial.
//
// If SignalMethod is set, the Offer/Answer exchange will be done automatically.
//
// Not thread-safe. Caller MUST hold the mutex before calling this function.
func (d *Dialer) NewPeerConnection(ctx context.Context) error {
	api := webrtc.NewAPI(webrtc.WithSettingEngine(d.settingEngine))
	peerConnection, err := api.NewPeerConnection(d.configuration)
	if err != nil {
		return err
	}
	peerConnection.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		d.mutex.Lock()
		defer d.mutex.Unlock()

		// TODO: handle this better
		if s == webrtc.PeerConnectionStateDisconnected {
			peerConnection.Close()
			d.peerConnection = nil
		}
	})

	d.peerConnection = peerConnection

	// Automatic Signalling when possible
	if d.SignalMethod != nil {
		var bChan chan bool = make(chan bool)

		// wait for local offer
		go func(blockingChan chan bool) {
			err := d.CreateOffer(ctx)
			blockingChan <- (err == nil)
		}(bChan)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case status := <-bChan:
			if !status {
				return errors.New("failed to create local offer")
			}
			offer, err := d.GetOffer()
			if err != nil {
				return err
			}
			err = d.SignalMethod.MakeOffer(offer)
			if err != nil {
				return err
			}
		}

		// wait for answer
		go func(blockingChan chan bool) {
			answerBytes, err := d.SignalMethod.GetAnswer()
			if err != nil {
				blockingChan <- false
				return
			}
			err = d.SetAnswer(answerBytes)
			blockingChan <- (err == nil)
		}(bChan)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case status := <-bChan:
			if !status {
				return errors.New("failed to receive answer")
			}
		}
	}

	return nil
}

// CreateOffer creates a local offer and sets it as the local description.
//
// Automatically called by NewPeerConction when SignalMethod is set.
func (d *Dialer) CreateOffer(ctx context.Context) error {
	localDescription, err := d.peerConnection.CreateOffer(nil)
	if err != nil {
		return err
	}

	// Create channel that is blocked until ICE Gathering is complete
	gatherComplete := webrtc.GatheringCompletePromise(d.peerConnection)

	// Sets the LocalDescription, and starts our UDP listeners
	err = d.peerConnection.SetLocalDescription(localDescription)
	if err != nil {
		return err
	}

	// Block until ICE Gathering is complete, disabling trickle ICE
	// we do this because we only can exchange one signaling message
	// in a production application you should exchange ICE Candidates via OnICECandidate
	// TODO: use OnICECandidate callback instead
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-gatherComplete:
		return nil
	}
}

// GetOffer returns the local offer.
//
// Automatically called by NewPeerConction when SignalMethod is set.
func (d *Dialer) GetOffer() ([]byte, error) {
	offer := d.peerConnection.LocalDescription()
	// offer to JSON bytes
	return json.Marshal(offer)
}

// SetAnswer sets the remote answer.
//
// Automatically called by NewPeerConction when SignalMethod is set.
func (d *Dialer) SetAnswer(answer []byte) error {
	// answer from JSON bytes
	answerUnmarshal := webrtc.SessionDescription{}
	err := json.Unmarshal(answer, &answerUnmarshal)
	if err != nil {
		return err
	}
	return d.peerConnection.SetRemoteDescription(answerUnmarshal)
}