package smgr

import (
	"fmt"
	"sync"

	"github.com/kubeedge/beehive/pkg/common/log"
	"github.com/kubeedge/viaduct/pkg/api"
	"github.com/kubeedge/viaduct/pkg/comm"
	"github.com/lucas-clemente/quic-go"
)

const (
	NumStreamsMax = 99
)

// get stream from session
// AcceptStream or OpenStreamXX
type GetFuncEx func(useType api.UseType) (*Stream, error)

type StreamManager struct {
	NumStreamsMax int
	Session       *Session

	messagePool PoolManager
	binaryPool  PoolManager
	lock        sync.RWMutex
}

type PoolManager struct {
	idlePool streamPool
	busyPool streamPool
	lock     sync.RWMutex
	cond     sync.Cond
}

type streamPool struct {
	streams map[quic.StreamID]*Stream
	lock    sync.RWMutex
}

func (pool *streamPool) addStream(stream *Stream) {
	pool.lock.Lock()
	defer pool.lock.Unlock()
	pool.streams[stream.Stream.StreamID()] = stream
}

func (pool *streamPool) len() int {
	pool.lock.RLock()
	defer pool.lock.RUnlock()
	return len(pool.streams)
}

func (pool *streamPool) delStream(stream quic.Stream) {
	pool.lock.Lock()
	defer pool.lock.Unlock()
	delete(pool.streams, stream.StreamID())
}

func (pool *streamPool) getStream() *Stream {
	pool.lock.Lock()
	defer pool.lock.Unlock()

	// get one stream randomly
	var stream *Stream
	for _, stream = range pool.streams {
		break
	}
	if stream != nil {
		delete(pool.streams, stream.Stream.StreamID())
	}
	return stream
}

func (pool *streamPool) freeStream() {
	pool.lock.Lock()
	defer pool.lock.Unlock()
	for _, stream := range pool.streams {
		stream.Stream.CancelRead(quic.ErrorCode(comm.StatusCodeFreeStream))
		stream.Stream.Close()
		stream.Stream.CancelWrite(quic.ErrorCode(comm.StatusCodeFreeStream))
	}
	pool.streams = nil
}

func (mgr *PoolManager) len() int {
	mgr.lock.RLock()
	defer mgr.lock.RUnlock()
	return mgr.idlePool.len() + mgr.busyPool.len()
}

func (mgr *PoolManager) acquireStream() (*Stream, error) {
	mgr.lock.Lock()
	defer mgr.lock.Unlock()
	// get a stream from idle pool
	stream := mgr.idlePool.getStream()
	if stream != nil {
		// add into busy pool
		mgr.busyPool.addStream(stream)
		return stream, nil
	}
	return nil, fmt.Errorf("no stream existing")
}

func (mgr *PoolManager) addBusyStream(stream *Stream) {
	mgr.lock.Lock()
	defer mgr.lock.Unlock()
	mgr.busyPool.addStream(stream)
}

func (mgr *PoolManager) addIdleStream(stream *Stream) {
	mgr.lock.Lock()
	defer mgr.lock.Unlock()
	mgr.idlePool.addStream(stream)
	mgr.cond.Signal()
}

func (mgr *PoolManager) availableOrWait() {
	mgr.lock.Lock()
	defer mgr.lock.Unlock()

	if mgr.idlePool.len() <= 0 {
		mgr.cond.Wait()
	}
}
 
// TODO: check and free the idle streams when the idle number
// is more 80% when releaseStream called
func (mgr *PoolManager) releaseStream(stream quic.Stream) {
	mgr.lock.Lock()
	defer mgr.lock.Unlock()

	// delete the stream from busy pool
	mgr.busyPool.delStream(stream)

	// add into idle pool
	mgr.idlePool.addStream(&Stream{
		Stream: stream,
	})
	mgr.cond.Signal()
}

func (mgr *PoolManager) Destroy() {
	mgr.lock.Lock()
	defer mgr.lock.Unlock()

	mgr.idlePool.freeStream()
	mgr.busyPool.freeStream()
}

func NewStreamManager(streamMax int, session quic.Session) *StreamManager {
	if streamMax <= 0 {
		streamMax = NumStreamsMax
	}

	streamMgr := &StreamManager{
		NumStreamsMax: streamMax,
		Session:       &Session{session},
		messagePool: PoolManager{
			idlePool: streamPool{
				streams: make(map[quic.StreamID]*Stream),
			},
			busyPool: streamPool{
				streams: make(map[quic.StreamID]*Stream),
			},
		},
		binaryPool: PoolManager{
			idlePool: streamPool{
				streams: make(map[quic.StreamID]*Stream),
			},
			busyPool: streamPool{
				streams: make(map[quic.StreamID]*Stream),
			},
		},
	}
	streamMgr.messagePool.cond.L = &streamMgr.messagePool.lock
	streamMgr.binaryPool.cond.L = &streamMgr.binaryPool.lock
	return streamMgr
}

func (mgr *StreamManager) getPoolManager(useType api.UseType) *PoolManager {
	var poolMgr *PoolManager
	switch useType {
	case api.UseTypeMessage:
		poolMgr = &mgr.messagePool
	case api.UseTypeStream:
		poolMgr = &mgr.binaryPool
	default:
		log.LOGGER.Errorf("bad stream use type(%s)%s, ", useType, api.UseTypeMessage)
	}
	return poolMgr
}

func (mgr *StreamManager) ReleaseStream(useType api.UseType, stream quic.Stream) {
	mgr.lock.Lock()
	defer mgr.lock.Unlock()

	// try to get stream from pool
	poolMgr := mgr.getPoolManager(useType)
	if poolMgr == nil {
		return
	}
	poolMgr.releaseStream(stream)
}

func (mgr *StreamManager) AddStream(stream *Stream) {
	mgr.lock.Lock()
	defer mgr.lock.Unlock()

	// try to get stream from pool
	poolMgr := mgr.getPoolManager(stream.UseType)
	if poolMgr == nil {
		return
	}
	poolMgr.addIdleStream(stream)
}

func (mgr *StreamManager) GetStream(useType api.UseType, getFuncEx GetFuncEx) (quic.Stream, error) {
	mgr.lock.Lock()
	// try to get stream from pool
	poolMgr := mgr.getPoolManager(useType)
	if poolMgr == nil {
		mgr.lock.Unlock()
		return nil, fmt.Errorf("bad stream use type(%s)", useType)
	}

	// get stream from stream pool
	if getFuncEx == nil {
		// wait if have no idle stream
		poolMgr.availableOrWait()
	} else {
		// check the max number of streams
		total := mgr.binaryPool.len() + mgr.messagePool.len()
		if total >= mgr.NumStreamsMax {
			// if no available idle stream, block and wait
			log.LOGGER.Debugf("wait for idle stream")
			mgr.lock.Unlock()
			// check it has a available stream or wait for a stream
			poolMgr.availableOrWait()
			mgr.lock.Lock()
		}
	}

	// acquire stream from current stream pool
	stream, err := poolMgr.acquireStream()
	if err == nil {
		mgr.lock.Unlock()
		return stream.Stream, err
	}
	mgr.lock.Unlock()

	// failed to acquire stream
	// return err if just want get stream from pool
	if getFuncEx == nil {
		return nil, fmt.Errorf("failed to get stream, error: %+v", err)
	}

	// try to get stream from session
	stream, err = getFuncEx(useType)
	if err != nil {
		log.LOGGER.Warnf("get stream error(%+v)", err)
		return nil, err
	}

	// add the new stream into pools
	mgr.lock.Lock()
	poolMgr.addBusyStream(stream)
	mgr.lock.Unlock()
	return stream.Stream, nil
}

func (mgr *StreamManager) Destroy() {
	mgr.lock.Lock()
	defer mgr.lock.Unlock()

	mgr.messagePool.Destroy()
	mgr.binaryPool.Destroy()
}
