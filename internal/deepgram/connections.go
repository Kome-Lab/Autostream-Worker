package deepgram

import (
	"context"
	"errors"
)

// A user and connection generation identify a speaker. SSRC is only retained
// for packets that arrive before Discord has resolved the speaking user.
type connectionKey struct {
	UserID         string
	Generation     uint64
	UnresolvedSSRC uint32
}

type dialCall struct {
	done   chan struct{}
	cancel context.CancelFunc
	conn   *speakerConnection
	err    error
}

func connectionKeyFor(packet AudioPacket) connectionKey {
	if packet.UserID != "" {
		return connectionKey{UserID: packet.UserID, Generation: packet.ConnectionGeneration}
	}
	return connectionKey{Generation: packet.ConnectionGeneration, UnresolvedSSRC: packet.SSRC}
}

func (s *Session) connection(ctx context.Context, packet AudioPacket) (*speakerConnection, error) {
	key := connectionKeyFor(packet)
	for {
		var stale []*speakerConnection
		var staleDials []*dialCall
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return nil, ErrClosed
		}
		if s.latestConnectionGeneration > 0 && packet.ConnectionGeneration < s.latestConnectionGeneration {
			s.mu.Unlock()
			return nil, ErrStaleConnectionGeneration
		}
		if packet.ConnectionGeneration > s.latestConnectionGeneration {
			s.latestConnectionGeneration = packet.ConnectionGeneration
			for candidateKey, call := range s.dialing {
				if candidateKey.Generation < packet.ConnectionGeneration {
					delete(s.dialing, candidateKey)
					staleDials = append(staleDials, call)
				}
			}
			for candidateKey, conn := range s.conns {
				if candidateKey.Generation < packet.ConnectionGeneration {
					delete(s.conns, candidateKey)
					stale = append(stale, conn)
				}
			}
		}
		if len(stale) > 0 || len(staleDials) > 0 {
			s.mu.Unlock()
			for _, call := range staleDials {
				call.cancel()
			}
			for _, conn := range stale {
				conn.abort()
			}
			continue
		}
		if s.terminalProviderErr != nil {
			err := s.terminalProviderErr
			s.mu.Unlock()
			return nil, err
		}
		if conn := s.conns[key]; conn != nil {
			s.mu.Unlock()
			return conn, nil
		}
		if packet.UserID != "" || packet.ConnectionGeneration != 0 {
			for candidateKey, conn := range s.conns {
				if candidateKey == key {
					continue
				}
				sameUser := packet.UserID != "" && candidateKey.UserID == packet.UserID
				sameSSRC := packet.SSRC != 0 && conn.ssrc == packet.SSRC
				if !sameUser && !sameSSRC {
					continue
				}
				delete(s.conns, candidateKey)
				stale = append(stale, conn)
			}
		}
		if len(stale) > 0 {
			s.mu.Unlock()
			for _, conn := range stale {
				conn.abort()
			}
			continue
		}
		if call := s.dialing[key]; call != nil {
			done := call.done
			s.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ErrUnavailable
			case <-done:
				if call.err != nil {
					return nil, call.err
				}
				if call.conn != nil {
					continue
				}
				return nil, ErrUnavailable
			}
		}
		if len(s.conns)+len(s.dialing) >= s.options.maxConnections {
			s.mu.Unlock()
			return nil, ErrUnavailable
		}
		dialCtx, cancel := context.WithTimeout(ctx, s.options.dialTimeout)
		call := &dialCall{done: make(chan struct{}), cancel: cancel}
		s.dialing[key] = call
		s.dialWG.Add(1)
		s.mu.Unlock()

		conn, err := s.completeDial(dialCtx, call, key, packet)
		cancel()
		s.dialWG.Done()
		return conn, err
	}
}

func (s *Session) completeDial(ctx context.Context, call *dialCall, key connectionKey, packet AudioPacket) (*speakerConnection, error) {
	sock, dialErr := s.dial(ctx)
	if dialErr != nil && !errors.Is(dialErr, context.Canceled) {
		errorClass := "provider_dial_failed"
		if classified, ok := SafeErrorClass(dialErr); ok {
			errorClass = classified
		}
		httpStatus, _ := SafeHTTPStatus(dialErr)
		s.recordProviderErrorWithStatus(errorClass, httpStatus)
	}
	var conn *speakerConnection
	var closeSocket bool

	s.mu.Lock()
	delete(s.dialing, key)
	switch {
	case dialErr != nil:
		call.err = dialErr
		if errors.Is(dialErr, ErrUnavailable) && !IsRetryable(dialErr) {
			s.terminalProviderErr = dialErr
		}
		closeSocket = sock != nil
	case s.closed:
		call.err = ErrClosed
		closeSocket = true
	case s.latestConnectionGeneration > 0 && key.Generation < s.latestConnectionGeneration:
		call.err = ErrStaleConnectionGeneration
		closeSocket = true
	default:
		conn = newSpeakerConnection(s, sock, key, packet)
		s.conns[key] = conn
		s.loopWG.Add(3)
		call.conn = conn
	}
	close(call.done)
	s.mu.Unlock()

	if closeSocket && sock != nil {
		_ = sock.CloseNow()
	}
	if conn != nil {
		conn.start()
		return conn, nil
	}
	return nil, call.err
}

func (s *Session) discard(conn *speakerConnection) {
	s.mu.Lock()
	if s.conns[conn.key] == conn {
		delete(s.conns, conn.key)
	}
	s.mu.Unlock()
	conn.abort()
}
