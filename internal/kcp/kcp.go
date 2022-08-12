package kcp

import (
	"encoding/binary"
	"sync/atomic"
	"time"
)

const (
	IKCP_RTO_NDL     = 30  // no delay min rto
	IKCP_RTO_MIN     = 100 // normal min rto
	IKCP_RTO_DEF     = 200
	IKCP_RTO_MAX     = 60000
	IKCP_CMD_PUSH    = 81 // cmd: push data          数据分片
	IKCP_CMD_ACK     = 82 // cmd: ack                ack分片
	IKCP_CMD_WASK    = 83 // cmd: window probe (ask) 请求告知窗口大小
	IKCP_CMD_WINS    = 84 // cmd: window size (tell) 告知窗口大小
	IKCP_ASK_SEND    = 1  // need to send IKCP_CMD_WASK
	IKCP_ASK_TELL    = 2  // need to send IKCP_CMD_WINS
	IKCP_WND_SND     = 32
	IKCP_WND_RCV     = 32
	IKCP_MTU_DEF     = 1400
	IKCP_ACK_FAST    = 3
	IKCP_INTERVAL    = 100
	IKCP_OVERHEAD    = 24
	IKCP_DEADLINK    = 20
	IKCP_THRESH_INIT = 2
	IKCP_THRESH_MIN  = 2
	IKCP_PROBE_INIT  = 7000   // 7 secs to probe window size
	IKCP_PROBE_LIMIT = 120000 // up to 120 secs to probe window
	IKCP_SN_OFFSET   = 12
)

// monotonic reference time point
var refTime time.Time = time.Now()

// currentMs returns current elapsed monotonic milliseconds since program startup
func currentMs() uint32 { return uint32(time.Since(refTime) / time.Millisecond) }

// output_callback is a prototype which ought capture conn and call conn.Write
type output_callback func(buf []byte, size int)

/* encode 8 bits unsigned int */
func ikcp_encode8u(p []byte, c byte) []byte {
	p[0] = c
	return p[1:]
}

/* decode 8 bits unsigned int */
func ikcp_decode8u(p []byte, c *byte) []byte {
	*c = p[0]
	return p[1:]
}

/* encode 16 bits unsigned int (lsb) */
func ikcp_encode16u(p []byte, w uint16) []byte {
	binary.LittleEndian.PutUint16(p, w)
	return p[2:]
}

/* decode 16 bits unsigned int (lsb) */
func ikcp_decode16u(p []byte, w *uint16) []byte {
	*w = binary.LittleEndian.Uint16(p)
	return p[2:]
}

/* encode 32 bits unsigned int (lsb) */
func ikcp_encode32u(p []byte, l uint32) []byte {
	binary.LittleEndian.PutUint32(p, l)
	return p[4:]
}

/* decode 32 bits unsigned int (lsb) */
func ikcp_decode32u(p []byte, l *uint32) []byte {
	*l = binary.LittleEndian.Uint32(p)
	return p[4:]
}

func _imin_(a, b uint32) uint32 {
	if a <= b {
		return a
	}
	return b
}

func _imax_(a, b uint32) uint32 {
	if a >= b {
		return a
	}
	return b
}

func _ibound_(lower, middle, upper uint32) uint32 {
	return _imin_(_imax_(lower, middle), upper)
}

func _itimediff(later, earlier uint32) int32 {
	return (int32)(later - earlier)
}

// segment defines a KCP segment
type segment struct {
	conv     uint32    //会话编号,和TCP的con一样,conv一致才会通信
	cmd      uint8     //指令类型 有4种
	frg      uint8     //标识segment分片ID,用户数据可能被分成多个kcp包发送,倒序->0
	wnd      uint16    //剩余接收窗口大小
	ts       uint32    //发送时刻的时间戳
	sn       uint32    //分片segment的序号,按1累加递增
	una      uint32    //待接收消息序号,代表编号前面的所有报都收到了的标志
	rto      uint32    //超时重传时间，根据网络去定
	xmit     uint32    //重传次数
	resendts uint32    //重传的时间戳。超过当前时间重发这个包
	fastack  uint32    //快速重传机制，记录被跳过的次数，超过次数进行快速重传
	acked    uint32    // mark if the seg has acked
	data     []byte    //数据内容
}

// encode a segment into buffer
func (seg *segment) encode(ptr []byte) []byte {
	ptr = ikcp_encode32u(ptr, seg.conv)
	ptr = ikcp_encode8u(ptr, seg.cmd)
	ptr = ikcp_encode8u(ptr, seg.frg)
	ptr = ikcp_encode16u(ptr, seg.wnd)
	ptr = ikcp_encode32u(ptr, seg.ts)
	ptr = ikcp_encode32u(ptr, seg.sn)
	ptr = ikcp_encode32u(ptr, seg.una)
	ptr = ikcp_encode32u(ptr, uint32(len(seg.data)))
	atomic.AddUint64(&DefaultSnmp.OutSegs, 1)
	return ptr
}

// KCP defines a single KCP connection
//conv:会话ID
//
//mtu:最大传输单元，默认数据是1400，最小是50
//
//mss:最大分片大小 不大于mtu
//
//state:连接状态 0xFFFFFFFF表示断开连接
//
//snd_una:第一个未确认的包
//
//snd_nxt:待发送包的序号
//
//rcv_nxt:待接收消息的序号。
//
//sshthresh:拥塞窗口的阈值，以包为单位(TCP以字节为单位)
//
//rx_rttval:ack接收rtt浮动值 代表连接的抖动情况
//
//rx_srtt:ack接收rtt平滑值(smoothed)
//
//rx_rto:由ACK接收延迟计算出来的重传超时时间
//
//rx_minrto:最小重传超时时间
//
//snd_wnd:发送窗口大小
//
//rcv_wnd:接收窗口大小
//
//rmt_wnd:远端接收窗口大小
//
//cwnd:拥塞窗口大小
//
//probe:探查变量。IKCP_ASK_TELL表示告知远端窗口大小，IKCP_ASK_SEND表示请求远端告知窗口大小
//
//current:当前的时间戳
//
//interval:内部flush刷新间隔
//
//ts_flush:下次flush刷新时间戳
//
//nodelay:是否启动无延迟模式
//
//updated:是否调用过update函数的标记(kcp需要上层通过不断的update和check来驱动kcp收发)
//
//ts_probe:下次探查窗口的时间戳
//
//probe_wait:探查窗口需要等待的时间
//
//dead_link:最大重传次数 超过了就被认为连接中断
//
//incr:可发送的最大数据量
//
//snd_queue:发送消息的队列
//
//rcv_queue:接收消息的队列
//
//snd_buf:发送消息的缓存
//
//rcv_buf:接收消息的缓存
//
//ack_list:等待发送的ack列表
//
//buffer:储存消息字节流的内存
//
//fastresend:触发快速重传的重复ack个数
//
//nocwnd：取消拥塞控制
//
//stream:是否采用流传输模式。Send的时候用到。为1的话 frg都是0，为0的话frg 倒序--->0
//
//reserved:保留的字节数，这个变量是kcp-go自己加的，目的是为加密和FEC这部分功能预留位置
type KCP struct {
	conv, mtu, mss, state                  uint32
	snd_una, snd_nxt, rcv_nxt              uint32
	ssthresh                               uint32
	rx_rttvar, rx_srtt                     int32
	rx_rto, rx_minrto                      uint32
	snd_wnd, rcv_wnd, rmt_wnd, cwnd, probe uint32
	interval, ts_flush                     uint32
	nodelay, updated                       uint32
	ts_probe, probe_wait                   uint32
	dead_link, incr                        uint32

	fastresend     int32
	nocwnd, stream int32

	snd_queue []segment
	rcv_queue []segment
	snd_buf   []segment
	rcv_buf   []segment

	acklist []ackItem

	buffer   []byte
	reserved int
	output   output_callback
}

type ackItem struct {
	sn uint32
	ts uint32
}

// NewKCP create a new kcp state machine
//
// 'conv' must be equal in the connection peers, or else data will be silently rejected.
//
// 'output' function will be called whenever these is data to be sent on wire.
func NewKCP(conv uint32, output output_callback) *KCP {
	kcp := new(KCP)
	kcp.conv = conv
	kcp.snd_wnd = IKCP_WND_SND
	kcp.rcv_wnd = IKCP_WND_RCV
	kcp.rmt_wnd = IKCP_WND_RCV
	kcp.mtu = IKCP_MTU_DEF
	kcp.mss = kcp.mtu - IKCP_OVERHEAD
	kcp.buffer = make([]byte, kcp.mtu)
	kcp.rx_rto = IKCP_RTO_DEF
	kcp.rx_minrto = IKCP_RTO_MIN
	kcp.interval = IKCP_INTERVAL
	kcp.ts_flush = IKCP_INTERVAL
	kcp.ssthresh = IKCP_THRESH_INIT
	kcp.dead_link = IKCP_DEADLINK
	kcp.output = output
	return kcp
}

// newSegment creates a KCP segment
func (kcp *KCP) newSegment(size int) (seg segment) {
	seg.data = xmitBuf.Get().([]byte)[:size]
	return
}

// delSegment recycles a KCP segment
func (kcp *KCP) delSegment(seg *segment) {
	if seg.data != nil {
		xmitBuf.Put(seg.data)
		seg.data = nil
	}
}

// ReserveBytes keeps n bytes untouched from the beginning of the buffer,
// the output_callback function should be aware of this.
//
// Return false if n >= mss
//      ReserveBytes是为上层引用的加密和FEC提供的，不考虑这些的话mss = mtu-head，为了实现加密和前向纠错就得给它预留空间，这样的话mss就得重置
func (kcp *KCP) ReserveBytes(n int) bool {
	if n >= int(kcp.mtu-IKCP_OVERHEAD) || n < 0 {
		return false
	}
	kcp.reserved = n
	kcp.mss = kcp.mtu - IKCP_OVERHEAD - uint32(n)
	return true
}

// PeekSize checks the size of next message in the recv queue
//         PeekSize是提前获取一个完整包的长度，通过frg来判断一个包是不是被分成了多片，如果有多个分片(比如3个)，frg是倒序来赋值的(frg分别是2 1 0)，这样的话碰到frg为0 就表示当前这个分片是最后一个分片。当queue里边没有数据 或者 有数据但是不能组成一个完整的包时，返回-1
func (kcp *KCP) PeekSize() (length int) {
	if len(kcp.rcv_queue) == 0 {
		return -1
	}

	seg := &kcp.rcv_queue[0]
	if seg.frg == 0 {
		return len(seg.data)
	}

	if len(kcp.rcv_queue) < int(seg.frg+1) {
		return -1
	}

	for k := range kcp.rcv_queue {
		seg := &kcp.rcv_queue[k]
		length += len(seg.data)
		if seg.frg == 0 {
			break
		}
	}
	return
}

// Receive data from kcp state machine
//
// Return number of bytes read.
//
// Return -1 when there is no readable data.
//
// Return -2 if len(buffer) is smaller than kcp.PeekSize().
//    Recv()主要是上层调用收数据的逻辑，先通过调用前边的PeekSize探测一个完整包的长度，如果rcv_queue没有包(peeksize=-1)或者queue里边完整包的数量超过了buff的剩余空间的话都返回错误码，正常情况下从rcv_queue中把data拷贝到buffer中，然后从rcv_queue中弹出已经copy好的数据，这个时候rcv_queue已经腾出空间了就从rcv_buf中拷贝一些合法数据到rcv_queue。
//
//        从这可以看出 读取数据的流程是 rcv_buf->rcv_queue->buffer。rcv_buf中数据包的sn是连续的，rcv_queue中如果数据包被分片了，那frg也是要连续的。
func (kcp *KCP) Recv(buffer []byte) (n int) {
	peeksize := kcp.PeekSize()
	if peeksize < 0 {
		return -1
	}

	if peeksize > len(buffer) {
		return -2
	}

	var fast_recover bool
	if len(kcp.rcv_queue) >= int(kcp.rcv_wnd) {
		fast_recover = true
	}

	// merge fragment
	count := 0
	for k := range kcp.rcv_queue {
		seg := &kcp.rcv_queue[k]
		copy(buffer, seg.data)
		buffer = buffer[len(seg.data):]
		n += len(seg.data)
		count++
		kcp.delSegment(seg)
		if seg.frg == 0 {
			break
		}
	}
	if count > 0 {
		kcp.rcv_queue = kcp.remove_front(kcp.rcv_queue, count)
	}

	// move available data from rcv_buf -> rcv_queue
	count = 0
	for k := range kcp.rcv_buf {
		seg := &kcp.rcv_buf[k]
		if seg.sn == kcp.rcv_nxt && len(kcp.rcv_queue)+count < int(kcp.rcv_wnd) {
			kcp.rcv_nxt++
			count++
		} else {
			break
		}
	}

	if count > 0 {
		kcp.rcv_queue = append(kcp.rcv_queue, kcp.rcv_buf[:count]...)
		kcp.rcv_buf = kcp.remove_front(kcp.rcv_buf, count)
	}

	// fast recover
	if len(kcp.rcv_queue) < int(kcp.rcv_wnd) && fast_recover {
		// ready to send back IKCP_CMD_WINS in ikcp_flush
		// tell remote my window size
		kcp.probe |= IKCP_ASK_TELL
	}
	return
}

// Send is user/upper level send, returns below zero for error
// Send是应用层把数据发送到KCP里边。如果kcp启用了stream模式，就看看snd_queue中最后一个seg里边还能不能加数据，如果len(seg.data)<kcp.mss就表示上一个seg中data里边还能塞一些数据，随后就是计算能塞多少的问题，extend的计算就是取 capacity和len(buffer)的最小值。现在已经计算出extend的值，那么把buffer拷贝进seg.data的时候就知道存在的具体位置了，然后计算剩余的buffer。
//
//        如果说剩余的buffer没有了，也就是完全塞到上一个seg里边了的话直接返回，本次send结束。如果还剩余有buffer，那么就要生成新的seg 然后进行相应的动作。计算剩余的buffer需要seg的个数count，count最小为1最大不能超过255，为啥不能超过255尼？因为frg的值跟count有关系，如果count>=256,那么第一个seg的frg就>=255了,而kcp的协议头里边frg是由一个字节长度存放的，最多只能存255，所以这里对于count的大小是有要求的。cout计算完了之后，剩余的操作就是创建seg然后把seg存放到snd_queue中。
func (kcp *KCP) Send(buffer []byte) int {
	var count int
	if len(buffer) == 0 {
		return -1
	}

	// append to previous segment in streaming mode (if possible)
	if kcp.stream != 0 {
		n := len(kcp.snd_queue)
		if n > 0 {
			seg := &kcp.snd_queue[n-1]
			if len(seg.data) < int(kcp.mss) {
				capacity := int(kcp.mss) - len(seg.data)
				extend := capacity
				if len(buffer) < capacity {
					extend = len(buffer)
				}

				// grow slice, the underlying cap is guaranteed to
				// be larger than kcp.mss
				oldlen := len(seg.data)
				seg.data = seg.data[:oldlen+extend]
				copy(seg.data[oldlen:], buffer)
				buffer = buffer[extend:]
			}
		}

		if len(buffer) == 0 {
			return 0
		}
	}

	if len(buffer) <= int(kcp.mss) {
		count = 1
	} else {
		count = (len(buffer) + int(kcp.mss) - 1) / int(kcp.mss)
	}

	if count > 255 {
		return -2
	}

	if count == 0 {
		count = 1
	}

	for i := 0; i < count; i++ {
		var size int
		if len(buffer) > int(kcp.mss) {
			size = int(kcp.mss)
		} else {
			size = len(buffer)
		}
		seg := kcp.newSegment(size)
		copy(seg.data, buffer[:size])
		if kcp.stream == 0 { // message mode
			seg.frg = uint8(count - i - 1)
		} else { // stream mode
			seg.frg = 0
		}
		kcp.snd_queue = append(kcp.snd_queue, seg)
		buffer = buffer[size:]
	}
	return 0
}

func (kcp *KCP) update_ack(rtt int32) {
	// https://tools.ietf.org/html/rfc6298
	var rto uint32
	if kcp.rx_srtt == 0 {
		kcp.rx_srtt = rtt
		kcp.rx_rttvar = rtt >> 1
	} else {
		delta := rtt - kcp.rx_srtt
		kcp.rx_srtt += delta >> 3
		if delta < 0 {
			delta = -delta
		}
		if rtt < kcp.rx_srtt-kcp.rx_rttvar {
			// if the new RTT sample is below the bottom of the range of
			// what an RTT measurement is expected to be.
			// give an 8x reduced weight versus its normal weighting
			kcp.rx_rttvar += (delta - kcp.rx_rttvar) >> 5
		} else {
			kcp.rx_rttvar += (delta - kcp.rx_rttvar) >> 2
		}
	}
	rto = uint32(kcp.rx_srtt) + _imax_(kcp.interval, uint32(kcp.rx_rttvar)<<2)
	kcp.rx_rto = _ibound_(kcp.rx_minrto, rto, IKCP_RTO_MAX)
}

func (kcp *KCP) shrink_buf() {
	if len(kcp.snd_buf) > 0 {
		seg := &kcp.snd_buf[0]
		kcp.snd_una = seg.sn
	} else {
		kcp.snd_una = kcp.snd_nxt
	}
}

func (kcp *KCP) parse_ack(sn uint32) {
	if _itimediff(sn, kcp.snd_una) < 0 || _itimediff(sn, kcp.snd_nxt) >= 0 {
		return
	}

	for k := range kcp.snd_buf {
		seg := &kcp.snd_buf[k]
		if sn == seg.sn {
			// mark and free space, but leave the segment here,
			// and wait until `una` to delete this, then we don't
			// have to shift the segments behind forward,
			// which is an expensive operation for large window
			seg.acked = 1
			kcp.delSegment(seg)
			break
		}
		if _itimediff(sn, seg.sn) < 0 {
			break
		}
	}
}

func (kcp *KCP) parse_fastack(sn, ts uint32) {
	if _itimediff(sn, kcp.snd_una) < 0 || _itimediff(sn, kcp.snd_nxt) >= 0 {
		return
	}

	for k := range kcp.snd_buf {
		seg := &kcp.snd_buf[k]
		if _itimediff(sn, seg.sn) < 0 {
			break
		} else if sn != seg.sn && _itimediff(seg.ts, ts) <= 0 {
			seg.fastack++
		}
	}
}

func (kcp *KCP) parse_una(una uint32) int {
	count := 0
	for k := range kcp.snd_buf {
		seg := &kcp.snd_buf[k]
		if _itimediff(una, seg.sn) > 0 {
			kcp.delSegment(seg)
			count++
		} else {
			break
		}
	}
	if count > 0 {
		kcp.snd_buf = kcp.remove_front(kcp.snd_buf, count)
	}
	return count
}

// ack append
func (kcp *KCP) ack_push(sn, ts uint32) {
	kcp.acklist = append(kcp.acklist, ackItem{sn, ts})
}

// returns true if data has repeated
func (kcp *KCP) parse_data(newseg segment) bool {
	sn := newseg.sn
	if _itimediff(sn, kcp.rcv_nxt+kcp.rcv_wnd) >= 0 ||
		_itimediff(sn, kcp.rcv_nxt) < 0 {
		return true
	}

	n := len(kcp.rcv_buf) - 1
	insert_idx := 0
	repeat := false
	for i := n; i >= 0; i-- {
		seg := &kcp.rcv_buf[i]
		if seg.sn == sn {
			repeat = true
			break
		}
		if _itimediff(sn, seg.sn) > 0 {
			insert_idx = i + 1
			break
		}
	}

	if !repeat {
		// replicate the content if it's new
		dataCopy := xmitBuf.Get().([]byte)[:len(newseg.data)]
		copy(dataCopy, newseg.data)
		newseg.data = dataCopy

		if insert_idx == n+1 {
			kcp.rcv_buf = append(kcp.rcv_buf, newseg)
		} else {
			kcp.rcv_buf = append(kcp.rcv_buf, segment{})
			copy(kcp.rcv_buf[insert_idx+1:], kcp.rcv_buf[insert_idx:])
			kcp.rcv_buf[insert_idx] = newseg
		}
	}

	// move available data from rcv_buf -> rcv_queue
	count := 0
	for k := range kcp.rcv_buf {
		seg := &kcp.rcv_buf[k]
		if seg.sn == kcp.rcv_nxt && len(kcp.rcv_queue)+count < int(kcp.rcv_wnd) {
			kcp.rcv_nxt++
			count++
		} else {
			break
		}
	}
	if count > 0 {
		kcp.rcv_queue = append(kcp.rcv_queue, kcp.rcv_buf[:count]...)
		kcp.rcv_buf = kcp.remove_front(kcp.rcv_buf, count)
	}

	return repeat
}

// Input a packet into kcp state machine.
//
// 'regular' indicates it's a real data packet from remote, and it means it's not generated from ReedSolomon
// codecs.
//
// 'ackNoDelay' will trigger immediate ACK, but surely it will not be efficient in bandwidth
func (kcp *KCP) Input(data []byte, regular, ackNoDelay bool) int {
	snd_una := kcp.snd_una
	if len(data) < IKCP_OVERHEAD {
		return -1
	}

	var latest uint32 // the latest ack packet
	var flag int
	var inSegs uint64
	var windowSlides bool

	for {
		var ts, sn, length, una, conv uint32
		var wnd uint16
		var cmd, frg uint8

		if len(data) < int(IKCP_OVERHEAD) {
			break
		}

		data = ikcp_decode32u(data, &conv)
		if conv != kcp.conv {
			return -1
		}

		data = ikcp_decode8u(data, &cmd)
		data = ikcp_decode8u(data, &frg)
		data = ikcp_decode16u(data, &wnd)
		data = ikcp_decode32u(data, &ts)
		data = ikcp_decode32u(data, &sn)
		data = ikcp_decode32u(data, &una)
		data = ikcp_decode32u(data, &length)
		if len(data) < int(length) {
			return -2
		}

		if cmd != IKCP_CMD_PUSH && cmd != IKCP_CMD_ACK &&
			cmd != IKCP_CMD_WASK && cmd != IKCP_CMD_WINS {
			return -3
		}

		// only trust window updates from regular packets. i.e: latest update
		if regular {
			kcp.rmt_wnd = uint32(wnd)
		}
		if kcp.parse_una(una) > 0 {
			windowSlides = true
		}
		kcp.shrink_buf()

		if cmd == IKCP_CMD_ACK {
			kcp.parse_ack(sn)
			kcp.parse_fastack(sn, ts)
			flag |= 1
			latest = ts
		} else if cmd == IKCP_CMD_PUSH {
			repeat := true
			if _itimediff(sn, kcp.rcv_nxt+kcp.rcv_wnd) < 0 {
				kcp.ack_push(sn, ts)
				if _itimediff(sn, kcp.rcv_nxt) >= 0 {
					var seg segment
					seg.conv = conv
					seg.cmd = cmd
					seg.frg = frg
					seg.wnd = wnd
					seg.ts = ts
					seg.sn = sn
					seg.una = una
					seg.data = data[:length] // delayed data copying
					repeat = kcp.parse_data(seg)
				}
			}
			if regular && repeat {
				atomic.AddUint64(&DefaultSnmp.RepeatSegs, 1)
			}
		} else if cmd == IKCP_CMD_WASK {
			// ready to send back IKCP_CMD_WINS in Ikcp_flush
			// tell remote my window size
			kcp.probe |= IKCP_ASK_TELL
		} else if cmd == IKCP_CMD_WINS {
			// do nothing
		} else {
			return -3
		}

		inSegs++
		data = data[length:]
	}
	atomic.AddUint64(&DefaultSnmp.InSegs, inSegs)

	// update rtt with the latest ts
	// ignore the FEC packet
	if flag != 0 && regular {
		current := currentMs()
		if _itimediff(current, latest) >= 0 {
			kcp.update_ack(_itimediff(current, latest))
		}
	}

	// cwnd update when packet arrived
	if kcp.nocwnd == 0 {
		if _itimediff(kcp.snd_una, snd_una) > 0 {
			if kcp.cwnd < kcp.rmt_wnd {
				mss := kcp.mss
				if kcp.cwnd < kcp.ssthresh {
					kcp.cwnd++
					kcp.incr += mss
				} else {
					if kcp.incr < mss {
						kcp.incr = mss
					}
					kcp.incr += (mss*mss)/kcp.incr + (mss / 16)
					if (kcp.cwnd+1)*mss <= kcp.incr {
						if mss > 0 {
							kcp.cwnd = (kcp.incr + mss - 1) / mss
						} else {
							kcp.cwnd = kcp.incr + mss - 1
						}
					}
				}
				if kcp.cwnd > kcp.rmt_wnd {
					kcp.cwnd = kcp.rmt_wnd
					kcp.incr = kcp.rmt_wnd * mss
				}
			}
		}
	}

	if windowSlides { // if window has slided, flush
		kcp.flush(false)
	} else if ackNoDelay && len(kcp.acklist) > 0 { // ack immediately
		kcp.flush(true)
	}
	return 0
}

func (kcp *KCP) wnd_unused() uint16 {
	if len(kcp.rcv_queue) < int(kcp.rcv_wnd) {
		return uint16(int(kcp.rcv_wnd) - len(kcp.rcv_queue))
	}
	return 0
}

// flush pending data
func (kcp *KCP) flush(ackOnly bool) uint32 {
	var seg segment
	seg.conv = kcp.conv
	seg.cmd = IKCP_CMD_ACK
	seg.wnd = kcp.wnd_unused()
	seg.una = kcp.rcv_nxt

	buffer := kcp.buffer
	ptr := buffer[kcp.reserved:] // keep n bytes untouched

	// makeSpace makes room for writing
	makeSpace := func(space int) {
		size := len(buffer) - len(ptr)
		if size+space > int(kcp.mtu) {
			kcp.output(buffer, size)
			ptr = buffer[kcp.reserved:]
		}
	}

	// flush bytes in buffer if there is any
	flushBuffer := func() {
		size := len(buffer) - len(ptr)
		if size > kcp.reserved {
			kcp.output(buffer, size)
		}
	}

	// flush acknowledges
	for i, ack := range kcp.acklist {
		makeSpace(IKCP_OVERHEAD)
		// filter jitters caused by bufferbloat
		if _itimediff(ack.sn, kcp.rcv_nxt) >= 0 || len(kcp.acklist)-1 == i {
			seg.sn, seg.ts = ack.sn, ack.ts
			ptr = seg.encode(ptr)
		}
	}
	kcp.acklist = kcp.acklist[0:0]

	if ackOnly { // flash remain ack segments
		flushBuffer()
		return kcp.interval
	}

	// probe window size (if remote window size equals zero)
	if kcp.rmt_wnd == 0 {
		current := currentMs()
		if kcp.probe_wait == 0 {
			kcp.probe_wait = IKCP_PROBE_INIT
			kcp.ts_probe = current + kcp.probe_wait
		} else {
			if _itimediff(current, kcp.ts_probe) >= 0 {
				if kcp.probe_wait < IKCP_PROBE_INIT {
					kcp.probe_wait = IKCP_PROBE_INIT
				}
				kcp.probe_wait += kcp.probe_wait / 2
				if kcp.probe_wait > IKCP_PROBE_LIMIT {
					kcp.probe_wait = IKCP_PROBE_LIMIT
				}
				kcp.ts_probe = current + kcp.probe_wait
				kcp.probe |= IKCP_ASK_SEND
			}
		}
	} else {
		kcp.ts_probe = 0
		kcp.probe_wait = 0
	}

	// flush window probing commands
	if (kcp.probe & IKCP_ASK_SEND) != 0 {
		seg.cmd = IKCP_CMD_WASK
		makeSpace(IKCP_OVERHEAD)
		ptr = seg.encode(ptr)
	}

	// flush window probing commands
	if (kcp.probe & IKCP_ASK_TELL) != 0 {
		seg.cmd = IKCP_CMD_WINS
		makeSpace(IKCP_OVERHEAD)
		ptr = seg.encode(ptr)
	}

	kcp.probe = 0

	// calculate window size
	cwnd := _imin_(kcp.snd_wnd, kcp.rmt_wnd)
	if kcp.nocwnd == 0 {
		cwnd = _imin_(kcp.cwnd, cwnd)
	}

	// sliding window, controlled by snd_nxt && sna_una+cwnd
	newSegsCount := 0
	for k := range kcp.snd_queue {
		if _itimediff(kcp.snd_nxt, kcp.snd_una+cwnd) >= 0 {
			break
		}
		newseg := kcp.snd_queue[k]
		newseg.conv = kcp.conv
		newseg.cmd = IKCP_CMD_PUSH
		newseg.sn = kcp.snd_nxt
		kcp.snd_buf = append(kcp.snd_buf, newseg)
		kcp.snd_nxt++
		newSegsCount++
	}
	if newSegsCount > 0 {
		kcp.snd_queue = kcp.remove_front(kcp.snd_queue, newSegsCount)
	}

	// calculate resent
	resent := uint32(kcp.fastresend)
	if kcp.fastresend <= 0 {
		resent = 0xffffffff
	}

	// check for retransmissions
	current := currentMs()
	var change, lostSegs, fastRetransSegs, earlyRetransSegs uint64
	minrto := int32(kcp.interval)

	ref := kcp.snd_buf[:len(kcp.snd_buf)] // for bounds check elimination
	for k := range ref {
		segment := &ref[k]
		needsend := false
		if segment.acked == 1 {
			continue
		}
		if segment.xmit == 0 { // initial transmit
			needsend = true
			segment.rto = kcp.rx_rto
			segment.resendts = current + segment.rto
		} else if segment.fastack >= resent { // fast retransmit
			needsend = true
			segment.fastack = 0
			segment.rto = kcp.rx_rto
			segment.resendts = current + segment.rto
			change++
			fastRetransSegs++
		} else if segment.fastack > 0 && newSegsCount == 0 { // early retransmit
			needsend = true
			segment.fastack = 0
			segment.rto = kcp.rx_rto
			segment.resendts = current + segment.rto
			change++
			earlyRetransSegs++
		} else if _itimediff(current, segment.resendts) >= 0 { // RTO
			needsend = true
			if kcp.nodelay == 0 {
				segment.rto += kcp.rx_rto
			} else {
				segment.rto += kcp.rx_rto / 2
			}
			segment.fastack = 0
			segment.resendts = current + segment.rto
			lostSegs++
		}

		if needsend {
			current = currentMs()
			segment.xmit++
			segment.ts = current
			segment.wnd = seg.wnd
			segment.una = seg.una

			need := IKCP_OVERHEAD + len(segment.data)
			makeSpace(need)
			ptr = segment.encode(ptr)
			copy(ptr, segment.data)
			ptr = ptr[len(segment.data):]

			if segment.xmit >= kcp.dead_link {
				kcp.state = 0xFFFFFFFF
			}
		}

		// get the nearest rto
		if rto := _itimediff(segment.resendts, current); rto > 0 && rto < minrto {
			minrto = rto
		}
	}

	// flash remain segments
	flushBuffer()

	// counter updates
	sum := lostSegs
	if lostSegs > 0 {
		atomic.AddUint64(&DefaultSnmp.LostSegs, lostSegs)
	}
	if fastRetransSegs > 0 {
		atomic.AddUint64(&DefaultSnmp.FastRetransSegs, fastRetransSegs)
		sum += fastRetransSegs
	}
	if earlyRetransSegs > 0 {
		atomic.AddUint64(&DefaultSnmp.EarlyRetransSegs, earlyRetransSegs)
		sum += earlyRetransSegs
	}
	if sum > 0 {
		atomic.AddUint64(&DefaultSnmp.RetransSegs, sum)
	}

	// cwnd update
	if kcp.nocwnd == 0 {
		// update ssthresh
		// rate halving, https://tools.ietf.org/html/rfc6937
		if change > 0 {
			inflight := kcp.snd_nxt - kcp.snd_una
			kcp.ssthresh = inflight / 2
			if kcp.ssthresh < IKCP_THRESH_MIN {
				kcp.ssthresh = IKCP_THRESH_MIN
			}
			kcp.cwnd = kcp.ssthresh + resent
			kcp.incr = kcp.cwnd * kcp.mss
		}

		// congestion control, https://tools.ietf.org/html/rfc5681
		if lostSegs > 0 {
			kcp.ssthresh = cwnd / 2
			if kcp.ssthresh < IKCP_THRESH_MIN {
				kcp.ssthresh = IKCP_THRESH_MIN
			}
			kcp.cwnd = 1
			kcp.incr = kcp.mss
		}

		if kcp.cwnd < 1 {
			kcp.cwnd = 1
			kcp.incr = kcp.mss
		}
	}

	return uint32(minrto)
}

// (deprecated)
//
// Update updates state (call it repeatedly, every 10ms-100ms), or you can ask
// ikcp_check when to call it again (without ikcp_input/_send calling).
// 'current' - current timestamp in millisec.
func (kcp *KCP) Update() {
	var slap int32

	current := currentMs()
	if kcp.updated == 0 {
		kcp.updated = 1
		kcp.ts_flush = current
	}

	slap = _itimediff(current, kcp.ts_flush)

	if slap >= 10000 || slap < -10000 {
		kcp.ts_flush = current
		slap = 0
	}

	if slap >= 0 {
		kcp.ts_flush += kcp.interval
		if _itimediff(current, kcp.ts_flush) >= 0 {
			kcp.ts_flush = current + kcp.interval
		}
		kcp.flush(false)
	}
}

// (deprecated)
//
// Check determines when should you invoke ikcp_update:
// returns when you should invoke ikcp_update in millisec, if there
// is no ikcp_input/_send calling. you can call ikcp_update in that
// time, instead of call update repeatly.
// Important to reduce unnacessary ikcp_update invoking. use it to
// schedule ikcp_update (eg. implementing an epoll-like mechanism,
// or optimize ikcp_update when handling massive kcp connections)
func (kcp *KCP) Check() uint32 {
	current := currentMs()
	ts_flush := kcp.ts_flush
	tm_flush := int32(0x7fffffff)
	tm_packet := int32(0x7fffffff)
	minimal := uint32(0)
	if kcp.updated == 0 {
		return current
	}

	if _itimediff(current, ts_flush) >= 10000 ||
		_itimediff(current, ts_flush) < -10000 {
		ts_flush = current
	}

	if _itimediff(current, ts_flush) >= 0 {
		return current
	}

	tm_flush = _itimediff(ts_flush, current)

	for k := range kcp.snd_buf {
		seg := &kcp.snd_buf[k]
		diff := _itimediff(seg.resendts, current)
		if diff <= 0 {
			return current
		}
		if diff < tm_packet {
			tm_packet = diff
		}
	}

	minimal = uint32(tm_packet)
	if tm_packet >= tm_flush {
		minimal = uint32(tm_flush)
	}
	if minimal >= kcp.interval {
		minimal = kcp.interval
	}

	return current + minimal
}

// SetMtu changes MTU size, default is 1400
func (kcp *KCP) SetMtu(mtu int) int {
	if mtu < 50 || mtu < IKCP_OVERHEAD {
		return -1
	}
	if kcp.reserved >= int(kcp.mtu-IKCP_OVERHEAD) || kcp.reserved < 0 {
		return -1
	}

	buffer := make([]byte, mtu)
	if buffer == nil {
		return -2
	}
	kcp.mtu = uint32(mtu)
	kcp.mss = kcp.mtu - IKCP_OVERHEAD - uint32(kcp.reserved)
	kcp.buffer = buffer
	return 0
}

// NoDelay options
// fastest: ikcp_nodelay(kcp, 1, 20, 2, 1)
// nodelay: 0:disable(default), 1:enable
// interval: internal update timer interval in millisec, default is 100ms
// resend: 0:disable fast resend(default), 1:enable fast resend
// nc: 0:normal congestion control(default), 1:disable congestion control
func (kcp *KCP) NoDelay(nodelay, interval, resend, nc int) int {
	if nodelay >= 0 {
		kcp.nodelay = uint32(nodelay)
		if nodelay != 0 {
			kcp.rx_minrto = IKCP_RTO_NDL
		} else {
			kcp.rx_minrto = IKCP_RTO_MIN
		}
	}
	if interval >= 0 {
		if interval > 5000 {
			interval = 5000
		} else if interval < 10 {
			interval = 10
		}
		kcp.interval = uint32(interval)
	}
	if resend >= 0 {
		kcp.fastresend = int32(resend)
	}
	if nc >= 0 {
		kcp.nocwnd = int32(nc)
	}
	return 0
}

// WndSize sets maximum window size: sndwnd=32, rcvwnd=32 by default
func (kcp *KCP) WndSize(sndwnd, rcvwnd int) int {
	if sndwnd > 0 {
		kcp.snd_wnd = uint32(sndwnd)
	}
	if rcvwnd > 0 {
		kcp.rcv_wnd = uint32(rcvwnd)
	}
	return 0
}

// WaitSnd gets how many packet is waiting to be sent
func (kcp *KCP) WaitSnd() int {
	return len(kcp.snd_buf) + len(kcp.snd_queue)
}

// remove front n elements from queue
// if the number of elements to remove is more than half of the size.
// just shift the rear elements to front, otherwise just reslice q to q[n:]
// then the cost of runtime.growslice can always be less than n/2
func (kcp *KCP) remove_front(q []segment, n int) []segment {
	if n > cap(q)/2 {
		newn := copy(q, q[n:])
		return q[:newn]
	}
	return q[n:]
}

// Release all cached outgoing segments
func (kcp *KCP) ReleaseTX() {
	for k := range kcp.snd_queue {
		if kcp.snd_queue[k].data != nil {
			xmitBuf.Put(kcp.snd_queue[k].data)
		}
	}
	for k := range kcp.snd_buf {
		if kcp.snd_buf[k].data != nil {
			xmitBuf.Put(kcp.snd_buf[k].data)
		}
	}
	kcp.snd_queue = nil
	kcp.snd_buf = nil
}
