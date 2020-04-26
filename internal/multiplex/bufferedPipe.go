// This is base on https://github.com/golang/go/blob/0436b162397018c45068b47ca1b5924a3eafdee0/src/net/net_fake.go#L173

package multiplex

import (
	"bytes"
	"errors"
	log "github.com/sirupsen/logrus"
	"io"
	"sync"
	"time"
)

const BUF_SIZE_LIMIT = 1 << 20 * 500

var ErrTimeout = errors.New("deadline exceeded")

// The point of a bufferedPipe is that Read() will block until data is available
type bufferedPipe struct {
	// only alloc when on first Read or Write
	buf *bytes.Buffer

	closed    bool
	rwCond    *sync.Cond
	rDeadline time.Time
	wtTimeout time.Duration
}

func NewBufferedPipe() *bufferedPipe {
	p := &bufferedPipe{
		rwCond: sync.NewCond(&sync.Mutex{}),
	}
	return p
}

func (p *bufferedPipe) Read(target []byte) (int, error) {
	log.Tracef("%p Read entering lock", p)
	p.rwCond.L.Lock()
	defer log.Tracef("%p Read exiting lock", p)
	defer p.rwCond.L.Unlock()
	if p.buf == nil {
		p.buf = new(bytes.Buffer)
	}
	for {
		log.Tracef("%p New Read loop cycle", p)
		if p.closed && p.buf.Len() == 0 {
			return 0, io.EOF
		}
		if !p.rDeadline.IsZero() {
			d := time.Until(p.rDeadline)
			if d <= 0 {
				return 0, ErrTimeout
			}
			time.AfterFunc(d, p.rwCond.Broadcast)
		}
		if p.buf.Len() > 0 {
			break
		}
		log.Tracef("%p Read waiting for broadcast and exiting lock", p)
		p.rwCond.Wait()
		log.Tracef("%p Read was woken up by broadcast and reacquired lock", p)
	}
	n, err := p.buf.Read(target)
	// err will always be nil because we have already verified that buf.Len() != 0
	log.Tracef("%p Read broadcasting to wake all waiting goroutines", p)
	p.rwCond.Broadcast()
	return n, err
}

func (p *bufferedPipe) WriteTo(w io.Writer) (n int64, err error) {
	log.Tracef("%p WriteTo entering lock", p)
	p.rwCond.L.Lock()
	defer log.Tracef("%p WriteTo exiting lock", p)
	defer p.rwCond.L.Unlock()
	if p.buf == nil {
		p.buf = new(bytes.Buffer)
	}
	for {
		log.Tracef("%p New WriteTo loop cycle", p)
		if p.closed && p.buf.Len() == 0 {
			return 0, io.EOF
		}
		if !p.rDeadline.IsZero() {
			d := time.Until(p.rDeadline)
			if d <= 0 {
				return 0, ErrTimeout
			}
			if p.wtTimeout == 0 {
				// if there hasn't been a scheduled broadcast
				time.AfterFunc(d, p.rwCond.Broadcast)
			}
		}
		if p.wtTimeout != 0 {
			p.rDeadline = time.Now().Add(p.wtTimeout)
			time.AfterFunc(p.wtTimeout, p.rwCond.Broadcast)
		}
		log.Tracef("%p WriteTo p.buf.Len(): %d", p, p.buf.Len())
		if p.buf.Len() > 0 {
			written, er := p.buf.WriteTo(w)
			n += written
			if er != nil {
				log.Tracef("%p WriteTo broadcasting with err %v", p, er)
				p.rwCond.Broadcast()
				return n, er
			}
			log.Tracef("%p WriteTo broadcasting to wake all waiting goroutines", p)
			p.rwCond.Broadcast()
		}
		log.Tracef("%p WriteTo waiting for broadcast and exiting lock", p)
		p.rwCond.Wait()
		log.Tracef("%p WriteTo was woken up by broadcast and reacquired lock", p)
	}
}

func (p *bufferedPipe) Write(input []byte) (int, error) {
	log.Tracef("%p Write entering lock", p)
	p.rwCond.L.Lock()
	defer log.Tracef("%p Write exiting lock", p)
	defer p.rwCond.L.Unlock()
	if p.buf == nil {
		p.buf = new(bytes.Buffer)
	}
	for {
		log.Tracef("%p New Write loop cycle", p)
		if p.closed {
			return 0, io.ErrClosedPipe
		}
		if p.buf.Len() <= BUF_SIZE_LIMIT {
			// if p.buf gets too large, write() will panic. We don't want this to happen
			break
		}
		log.Tracef("%p Write waiting for broadcast and exiting lock", p)
		p.rwCond.Wait()
		log.Tracef("%p Write was woken up by broadcast and reacquired lock", p)
	}
	n, err := p.buf.Write(input)
	// err will always be nil
	log.Tracef("%p Write broadcasting to wake all waiting goroutines", p)
	p.rwCond.Broadcast()
	return n, err
}

func (p *bufferedPipe) Close() error {
	log.Tracef("%p Close Entering bufferedPipe rwCond lock", p)
	p.rwCond.L.Lock()
	defer log.Tracef("%p Close Exiting bufferedPipe lock", p)
	defer p.rwCond.L.Unlock()

	p.closed = true
	p.rwCond.Broadcast()
	return nil
}

func (p *bufferedPipe) SetReadDeadline(t time.Time) {
	p.rwCond.L.Lock()
	defer p.rwCond.L.Unlock()

	p.rDeadline = t
	p.rwCond.Broadcast()
}

func (p *bufferedPipe) SetWriteToTimeout(d time.Duration) {
	p.rwCond.L.Lock()
	defer p.rwCond.L.Unlock()

	p.wtTimeout = d
	p.rwCond.Broadcast()
}
