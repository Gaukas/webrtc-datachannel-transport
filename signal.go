package transportc

import (
	"errors"
	"math/rand"
	"sync"
	"time"
)

var (
	// ErrOfferNotReady is returned by ReadOffer when no offer is available.
	ErrOfferNotReady = errors.New("offer not ready")

	// ErrInvalidOfferID is returned by Answer/ReadAnswer when the offer ID is invalid.
	ErrInvalidOfferID = errors.New("invalid offer ID")

	// ErrAnswerNotReady is returned by ReadAnswer when the offerID is valid but
	// an associated answer is not received yet.
	ErrAnswerNotReady = errors.New("answer not ready")
)

// Signal defines the interface for signalling, i.e., exchanging SDP offers and answers
// between two peers.
type Signal interface {
	// Offer submits a SDP offer generated by offerer to be read by the answerer.
	//
	// The caller is expected to keep the offerID for as a reference to the offer
	// when retrieving the answer later.
	Offer(offer []byte) (offerID uint64, err error)

	// ReadOffer reads the next SDP offer from the answerer.
	//
	// If no offer is available, ReadOffer may block until an offer is available
	// or return ErrOfferNotReady.
	ReadOffer() (offerID uint64, offer []byte, err error)

	// Answer submits a SDP answer generated by answerer to be read by the offerer.
	//
	// The caller is expected to provide the offerID returned by ReadOffer in order to
	// associate the answer with a previously submitted offer.
	Answer(offerID uint64, answer []byte) error

	// ReadAnswer reads the answer associated with the offerID.
	//
	// If an associated answer is not available, ReadAnswer may block until an answer
	// is available or return ErrAnswerNotReady.
	ReadAnswer(offerID uint64) ([]byte, error)
}

// DebugSignal implements a minimalistic signaling method used for debugging purposes.
type DebugSignal struct {
	offers      chan offer
	answers     map[uint64][]byte
	answerMutex sync.Mutex
}

type offer struct {
	id   uint64
	body []byte
}

// NewDebugSignal creates a new DebugSignal.
func NewDebugSignal(bufferSize int) *DebugSignal {
	return &DebugSignal{
		offers:  make(chan offer, bufferSize),
		answers: make(map[uint64][]byte),
	}
}

// Offer implements Signal.Offer.
// It writes the SDP offer to offers channel.
func (ds *DebugSignal) Offer(offerBody []byte) (uint64, error) {
	id := rand.Uint64() // skipcq: GSC-G404

	ds.offers <- offer{
		id:   id,
		body: offerBody,
	}
	return id, nil
}

// ReadOffer implements Signal.ReadOffer
// It reads the SDP offer from offers channel.
func (ds *DebugSignal) ReadOffer() (uint64, []byte, error) {
	if len(ds.offers) == 0 {
		return 0, nil, ErrOfferNotReady
	}
	offer := <-ds.offers
	return offer.id, offer.body, nil
}

// Answer implements Signal.Answer.
// It writes the SDP answer to answers channel.
func (ds *DebugSignal) Answer(offerID uint64, answer []byte) error {
	ds.answerMutex.Lock()
	defer ds.answerMutex.Unlock()

	// make sure the offerID is unique
	if _, ok := ds.answers[offerID]; ok {
		return ErrInvalidOfferID // offerID already used
	}

	ds.answers[offerID] = answer
	return nil
}

// ReadAnswer implements Signal.ReadAnswer
// It reads the SDP answer from answers channel.
func (ds *DebugSignal) ReadAnswer(offerID uint64) ([]byte, error) {
	ds.answerMutex.Lock()
	defer ds.answerMutex.Unlock()

	answer, ok := ds.answers[offerID]
	for !ok { // block until the answer is available
		ds.answerMutex.Unlock()
		// return ErrAnswerNotReady // an alternative non-blocking behavior
		time.Sleep(time.Millisecond * 50)
		ds.answerMutex.Lock()
		answer, ok = ds.answers[offerID]
	}
	// delete the answer so it can't be used again
	delete(ds.answers, offerID)

	return answer, nil
}