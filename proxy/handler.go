package proxy

import (
	"io"
	"sync"
	"sync/atomic"
	"time"

	"overlord/pkg/log"
	libnet "overlord/pkg/net"
	"overlord/pkg/prom"
	"overlord/pkg/types"
	"overlord/proxy/proto"
	"overlord/proxy/proto/memcache"
	mcbin "overlord/proxy/proto/memcache/binary"
	"overlord/proxy/proto/redis"
	rclstr "overlord/proxy/proto/redis/cluster"

	"github.com/pkg/errors"
)

const (
	handlerOpening = int32(0)
	handlerClosed  = int32(1)
)

// variables need to change
var (
	// TODO: config and reduce to small
	concurrent    = 2
	maxConcurrent = 1024
)

// Handler handle conn.
type Handler struct {
	p  *Proxy
    ClusterID int32
    Name string
	CacheType types.CacheType
	ListenAddr string
    closeWhenChange bool

	conn *libnet.Conn
	pc   proto.ProxyConn

	closed int32
	err    error
}

// NewHandler new a conn handler.
func NewHandler(p *Proxy, cc *ClusterConfig, client *libnet.Conn) (h *Handler) {
	h = &Handler{
		p:         p,
		ClusterID: cc.ID,
        Name:      cc.Name,
        CacheType: cc.CacheType,
        ListenAddr: cc.ListenAddr,
        closeWhenChange: cc.CloseWhenChange,
        conn: client,
	}
	// cache type
	switch h.CacheType {
	case types.CacheTypeMemcache:
		h.pc = memcache.NewProxyConn(h.conn)
	case types.CacheTypeMemcacheBinary:
		h.pc = mcbin.NewProxyConn(h.conn)
	case types.CacheTypeRedis:
		h.pc = redis.NewProxyConn(h.conn)
	case types.CacheTypeRedisCluster:
		h.pc = rclstr.NewProxyConn(h.conn)
	default:
		panic(types.ErrNoSupportCacheType)
	}
	prom.ConnIncr(h.Name)
	return
}

// Handle reads Msg from client connection and dispatchs Msg back to cache servers,
// then reads response from cache server and writes response into client connection.
func (h *Handler) Handle() {
	go h.handle()
}

func (h *Handler) handle() {
	var (
		messages []*proto.Message
		msgs     []*proto.Message
		wg       = &sync.WaitGroup{}
		err      error
        forwarder proto.Forwarder;
	)
	messages = h.allocMaxConcurrent(wg, messages, len(msgs))
    log.Infof("handler try to get forwarder for cluster:%d\n", h.ClusterID)
    forwarder = h.p.GetForwarder(h.ClusterID);
	for {
		// 1. read until limit or error
		if msgs, err = h.pc.Decode(messages); err != nil {
            log.Warnf("conn:%d failed to decode message, get error:%s", h.conn.ID, err.Error())
			h.deferHandle(messages, err)
            forwarder.Release()
			return
		}
        var fState = forwarder.State()
        if (ForwarderStateClosed == fState) {
            forwarder.Release()
            if (h.closeWhenChange) {
                log.Infof("cluster:%d is changed, need to close connection with client", h.ClusterID)
                h.deferHandle(messages, err)
                return
            }
            log.Infof("cluster:%d is changed, just use new cluster", h.ClusterID)
            forwarder = h.p.GetForwarder(h.ClusterID)
        }
        // here, forwwarder maybe get twice in redis cluster case, which will get in Encode process
		var err = forwarder.Forward(msgs)
        if (err != nil) {
            log.Warnf("forwarder of cluster:%d is close, need to reconnect", h.ClusterID)
            h.deferHandle(messages, err)
            forwarder.Release()
            return
        }
		wg.Wait()
		// 3. encode
		for _, msg := range msgs {
			if err = h.pc.Encode(msg, forwarder); err != nil {
				h.pc.Flush()
                log.Warnf("failed to encode message, close front connection now")
				h.deferHandle(messages, err)
                forwarder.Release()
				return
			}
			msg.MarkEnd()
			msg.ResetSubs()
			if prom.On {
				prom.ProxyTime(h.Name, msg.Request().CmdString(), int64(msg.TotalDur()/time.Microsecond))
			}
		}
		if err = h.pc.Flush(); err != nil {
            log.Warnf("failed to flush message, close front connection now")
			h.deferHandle(messages, err)
            forwarder.Release()
			return
		}
		// 4. release resource
		for _, msg := range msgs {
			msg.Reset()
		}
		// 5. alloc MaxConcurrent
		messages = h.allocMaxConcurrent(wg, messages, len(msgs))
	}
}

func (h *Handler) allocMaxConcurrent(wg *sync.WaitGroup, msgs []*proto.Message, lastCount int) []*proto.Message {
	var alloc int
	if lm := len(msgs); lm == 0 {
		alloc = concurrent
	} else if lm < maxConcurrent && lm == lastCount {
		alloc = lm * concurrent
	}
	if alloc > 0 {
		proto.PutMsgs(msgs)
		msgs = proto.GetMsgs(alloc) // TODO: change the msgs by lastCount trending
		for _, msg := range msgs {
			msg.WithWaitGroup(wg)
		}
	}
	return msgs
}

func (h *Handler) deferHandle(msgs []*proto.Message, err error) {
    h.p.RemoveConnection(h.ClusterID, h.conn.ID)
	proto.PutMsgs(msgs)
	h.closeWithError(err)
	return
}

func (h *Handler) closeWithError(err error) {
	if atomic.CompareAndSwapInt32(&h.closed, handlerOpening, handlerClosed) {
		h.err = err
		_ = h.conn.Close()
		atomic.AddInt32(&h.p.conns, -1) // NOTE: decr!!!
		if prom.On {
			prom.ConnDecr(h.Name)
		}
		if log.V(2) && errors.Cause(err) != io.EOF {
			log.Warnf("cluster(%s) addr(%s) remoteAddr(%s) handler close error:%+v", h.Name, h.ListenAddr, h.conn.RemoteAddr(), err)
		}
	}
}
