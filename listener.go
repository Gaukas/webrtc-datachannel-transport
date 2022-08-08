package transportc

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v3"
)

type ListenerRunningStatus = uint32

const (
	LISTENER_NEW ListenerRunningStatus = iota
	LISTENER_RUNNING
	LISTENER_SUSPENDED
	LISTENER_STOPPED
)

const ()

type Listener struct {
	runningStatus ListenerRunningStatus // Initialized at creation. Atomic. Access via sync/atomic methods only
	rand          *rand.Rand            // Initialized at creation.

	SignalMethod     SignalMethod
	MaxReadSize      int
	MaxAcceptTimeout time.Duration

	// WebRTC configuration
	settingEngine webrtc.SettingEngine
	configuration webrtc.Configuration

	// WebRTC PeerConnection
	mutex           sync.Mutex                        // mutex makes peerConnection thread-safe
	peerConnections map[uint64]*webrtc.PeerConnection // PCID:PeerConnection pair

	// chan Conn for Accept
	conns       chan net.Conn // Initialized at creation
	abortAccept chan bool     // Initialized at creation
}

func (l *Listener) Accept() (net.Conn, error) {
	// read next from conns
	select {
	case conn := <-l.conns:
		return conn, nil
	case <-l.abortAccept:
		return nil, errors.New("listener stopped")
	}
}

// Stop the listener. Close existing PeerConnections.
func (l *Listener) Stop() error {
	atomic.StoreUint32(&l.runningStatus, LISTENER_STOPPED)
	l.mutex.Lock()
	defer l.mutex.Unlock()
	for _, pc := range l.peerConnections {
		pc.Close()
	}
	l.peerConnections = make(map[uint64]*webrtc.PeerConnection) // clear map

	return nil
}

// Suspend the listener. Don't close existing PeerConnections.
func (l *Listener) Suspend() error {
	atomic.StoreUint32(&l.runningStatus, LISTENER_SUSPENDED)
	return nil
}

// startAcceptLoop() should be called before the first Accept() call.
func (l *Listener) startAcceptLoop() {
	if l.SignalMethod == SignalMethodManual {
		return // nothing to do for manual signaling (nil)
	}

	// Loop: accept new Offers from SignalMethod and establish new PeerConnections
	go func() {
		for atomic.LoadUint32(&l.runningStatus) != LISTENER_STOPPED { // new/running/suspended
			for atomic.LoadUint32(&l.runningStatus) == LISTENER_RUNNING {
				// Accept new Offer from SignalMethod
				offer, err := l.SignalMethod.GetOffer()
				if err != nil {
					continue
				}
				// Create new PeerConnection in a goroutine
				go func() {
					ctxTimeout, cancel := context.WithTimeout(context.Background(), l.MaxAcceptTimeout)
					defer cancel()
					l.nextPeerConnection(ctxTimeout, offer)
				}()
			}
			// sleep for a little while if new/suspended
			time.Sleep(time.Second)
		}
	}()
}

func (l *Listener) nextPeerConnection(ctx context.Context, offer []byte) error {
	api := webrtc.NewAPI(webrtc.WithSettingEngine(l.settingEngine))
	peerConnection, err := api.NewPeerConnection(l.configuration)
	if err != nil {
		return err
	}

	// Get a random ID
	id := l.nextPCID()
	l.mutex.Lock()
	l.peerConnections[id] = peerConnection
	l.mutex.Unlock()

	peerConnection.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		l.mutex.Lock()
		defer l.mutex.Unlock()

		// TODO: handle this better
		if s == webrtc.PeerConnectionStateDisconnected {
			peerConnection.Close()
			delete(l.peerConnections, id)
		}
	})

	peerConnection.OnDataChannel(func(d *webrtc.DataChannel) {
		d.OnOpen(func() {
			// detach from wrapper
			dc, err := d.Detach()
			if err != nil {
				return
			} else {
				conn := &Conn{
					dataChannel:       dc,
					readMaxPacketSize: l.MaxReadSize,
					readBuf:           make(chan []byte),
				}
				go conn.readLoop()
				l.conns <- conn
			}
		})

		d.OnClose(func() {
			// TODO: possibly tear down the PeerConnection if it is the last DataChannel?
		})
	})

	var bChan chan bool = make(chan bool)

	offerUnmarshal := webrtc.SessionDescription{}
	err = json.Unmarshal(offer, offerUnmarshal)
	err = peerConnection.SetRemoteDescription(offerUnmarshal)

	// wait for local answer
	go func(blockingChan chan bool) {
		localDescription, err := peerConnection.CreateAnswer(nil)
		if err != nil {
			blockingChan <- false
		}
		// Create channel that is blocked until ICE Gathering is complete
		gatherComplete := webrtc.GatheringCompletePromise(peerConnection)

		// Sets the LocalDescription, and starts our UDP listeners
		err = peerConnection.SetLocalDescription(localDescription)
		if err != nil {
			blockingChan <- false
		}
		<-gatherComplete
		blockingChan <- true
	}(bChan)

	select {
	case <-ctx.Done():
		return ctx.Err()
	case status := <-bChan:
		if !status {
			return errors.New("failed to create local answer")
		}
		answer := peerConnection.LocalDescription()
		// answer to JSON bytes
		answerBytes, err := json.Marshal(answer)
		if err != nil {
			return err
		}
		err = l.SignalMethod.Answer(answerBytes)
		if err != nil {
			return err
		}
	}

	return nil
}

// randomize a uint64 for ID. Must not conflict with existing IDs.
func (l *Listener) nextPCID() uint64 {
	l.mutex.Lock()
	defer l.mutex.Unlock()

	var id uint64
	for {
		id = uint64(rand.Uint64())
		if _, ok := l.peerConnections[id]; !ok { // not found
			break // okay to use this ID
		}
	}
	return id
}