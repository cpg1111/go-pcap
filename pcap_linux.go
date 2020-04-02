package pcap

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"time"
	"unsafe"

	"golang.org/x/net/bpf"
	syscall "golang.org/x/sys/unix"

	"github.com/google/gopacket"
	log "github.com/sirupsen/logrus"
)

const (
	//defaultFrameSize = 4096
	defaultFrameSize = 65632
	//defaultBlockNumbers = 128
	defaultBlockNumbers = 32
	//defaultBlockSize = defaultFrameSize * defaultBlockNumbers
	defaultBlockSize = 131072
	//defaultFramesPerBlock = defaultBlockSize / defaultFrameSize
	defaultFramesPerBlock = 32
	EthHlen               = 0x10
	// defaultSyscalls default setting for using syscalls
	defaultSyscalls     = false
	offsetToBlockStatus = 4 + 4
)

var (
	packetRALLSize           int32
	alignedTpacketHdrSize    int32
	alignedTpacketRALLSize   int32
	alignedTpacketAllHdrSize int32
)

type blockHeader struct {
	Version      uint32
	OffsetToPriv uint32
	H1           syscall.TpacketHdrV1
}

type captured struct {
	data []byte
	ci   gopacket.CaptureInfo
}

type Handle struct {
	syscalls        bool
	promiscuous     bool
	index           int
	snaplen         int32
	fd              int
	ring            []byte
	framePtr        int
	framesPerBuffer uint32
	frameIndex      uint32
	frameSize       uint32
	frameNumbers    uint32
	blockNumbers    int
	blockSize       int
	pollfd          []syscall.PollFd
	endian          binary.ByteOrder
	filter          []bpf.RawInstruction
	cache           []captured
}

func (h *Handle) ReadPacketData() (data []byte, ci gopacket.CaptureInfo, err error) {
	if h.syscalls {
		return h.readPacketDataSyscall()
	}
	// mmap can return multiple packets, so we can cache extras, and return if there are

	// if there already was one in the cache, return it
	if len(h.cache) > 0 {
		cap := h.cache[0]
		h.cache = h.cache[1:]
		return cap.data, cap.ci, nil
	}
	// there was not, so read a new one
	caps, err := h.readPacketDataMmap()
	if err != nil {
		return nil, ci, err
	}
	switch len(caps) {
	case 0:
		return nil, ci, nil
	case 1:
		return caps[0].data, caps[0].ci, nil
	}
	h.cache = caps
	cap := h.cache[0]
	h.cache = h.cache[1:]
	return cap.data, cap.ci, nil
}

func (h *Handle) readPacketDataSyscall() (data []byte, ci gopacket.CaptureInfo, err error) {
	b := make([]byte, h.snaplen)
	read, _, err := syscall.Recvfrom(h.fd, b, 0)
	if err != nil {
		return nil, ci, fmt.Errorf("error reading: %v", err)
	}
	// TODO: add CaptureInfo, specifically:
	//    capture timestamp
	//    original packet length
	ci = gopacket.CaptureInfo{
		CaptureLength:  read,
		InterfaceIndex: h.index,
	}
	return b, ci, nil
}

func (h *Handle) readPacketDataMmap() ([]captured, error) {
	logger := log.WithFields(log.Fields{
		"func":   "readPacketDataMmap",
		"method": "mmap",
	})
	logger.Debugf("started: framesPerBuffer %d, blockSize %d, frameSize %d, frameNumbers %d, blockNumbers %d", h.framesPerBuffer, h.blockSize, h.frameSize, h.frameNumbers, h.blockNumbers)
	// we check the bit setting on the pointer
	blockBase := h.framePtr * h.blockSize
	logger.Debugf("checking for packet at block %d, buffer position %d", h.framePtr, blockBase)
	if h.ring[blockBase+offsetToBlockStatus]&syscall.TP_STATUS_USER != syscall.TP_STATUS_USER {
		logger.Debugf("packet not ready at block %d position %d, polling via %#v", h.framePtr, blockBase, h.pollfd)
		val, err := syscall.Poll(h.pollfd, -1)
		logger.Debug("poll returned")
		if err != nil {
			logger.Errorf("error polling socket: %v", err)
			return nil, fmt.Errorf("error polling socket: %v", err)
		}
		if val == -1 {
			logger.Error("negative return value from polling socket")
			return nil, errors.New("negative return value from polling socket")
		}
		// socket was ready, so read from the mmap now
	}
	// read the header
	logger.Debugf("reading block header from position %d to position %d", blockBase, blockBase+h.blockSize)
	b := h.ring[blockBase : blockBase+h.blockSize]
	buf := bytes.NewBuffer(b[:])
	bHdr := blockHeader{}
	logger.Debugf("binary parsing block header of size %d", buf.Len())
	if err := binary.Read(buf, h.endian, &bHdr); err != nil {
		logger.Errorf("error reading block header: %v", err)
		return nil, fmt.Errorf("error reading block header: %v", err)
	}
	logger.Debugf("block header %#v", bHdr)
	// now we need to get the packets themselves
	numPkts := int(bHdr.H1.Num_pkts)
	packets := make([]captured, numPkts)

	nextOffset := bHdr.H1.Offset_to_first_pkt
	for i := 0; i < numPkts; i++ {
		hdr := syscall.Tpacket3Hdr{}
		b = b[nextOffset:]
		buf := bytes.NewBuffer(b[:alignedTpacketHdrSize])
		if err := binary.Read(buf, h.endian, &hdr); err != nil {
			msg := fmt.Sprintf("error reading tpacket3 header on byte %d: %v", i, err)
			logger.Errorf(msg)
			return nil, fmt.Errorf(msg)
		}
		logger.Debugf("tpacket3 header %#v", hdr)
		nextOffset = hdr.Next_offset

		// read the sockaddr_ll
		// unfortunately, we cannot do binary.Read() because syscall.SockaddrLinklayer has an embedded slice
		// so we have to read it manually
		// use b[hdr.Mac:hdr.Mac+alignedTpacketRALLSize] instead?
		sall, err := parseSocketAddrLinkLayer(b[alignedTpacketHdrSize:alignedTpacketAllHdrSize], h.endian)
		if err != nil {
			logger.Errorf("error parsing sockaddr_ll: %v", err)
			return nil, fmt.Errorf("error parsing sockaddr_ll for packet %d: %v", i, err)
		}

		ci := gopacket.CaptureInfo{
			Length:         int(hdr.Len),
			CaptureLength:  int(hdr.Snaplen),
			Timestamp:      time.Unix(int64(hdr.Sec), int64(hdr.Nsec)),
			InterfaceIndex: int(sall.Ifindex),
		}
		data := b[hdr.Mac : uint32(hdr.Mac)+hdr.Snaplen]
		packets[i] = captured{
			ci:   ci,
			data: data,
		}

		logger.Debugf("raw packet for packet %d: %d\n ", i, data)
	}

	// indicate we are done with this frame, send back to the kernel
	logger.Debugf("returning block at pos %d to kernel", h.framePtr)
	h.ring[blockBase+offsetToBlockStatus] = syscall.TP_STATUS_KERNEL

	h.framePtr = (h.framePtr + 1) % h.blockNumbers
	logger.Debugf("final block: %d", h.framePtr)

	return packets, nil
}

// Close close sockets and release resources
func (h *Handle) Close() {
	// close the socket
	_ = syscall.Close(h.fd)
	if h.ring != nil {
		_ = syscall.Munmap(h.ring)
	}
}

// set a classic BPF filter on the listener. filter must be compliant with
// tcpdump syntax.
func (h *Handle) setFilter() error {

	/*
	 * Try to install the kernel filter.
	 */
	prog := syscall.SockFprog{
		Len:    uint16(len(h.filter)),
		Filter: (*syscall.SockFilter)(unsafe.Pointer(&h.filter[0])),
	}

	if err := syscall.SetsockoptSockFprog(h.fd, syscall.SOL_SOCKET, syscall.SO_ATTACH_FILTER, &prog); err != nil {
		return fmt.Errorf("unable to set filter: %v", err)
	}
	return nil
}

func tpacketAlign(base int32) int32 {
	return (base + syscall.TPACKET_ALIGNMENT - 1) &^ (syscall.TPACKET_ALIGNMENT - 1)
}

func openLive(iface string, snaplen int32, promiscuous bool, timeout time.Duration, syscalls bool) (handle *Handle, _ error) {
	logger := log.WithFields(log.Fields{
		"func":        "openLive",
		"iface":       iface,
		"snaplen":     snaplen,
		"promiscuous": promiscuous,
		"timeout":     timeout,
		"syscalls":    syscalls,
	})
	logger.Debug("started")
	h := Handle{
		snaplen:  snaplen,
		syscalls: syscalls,
	}
	// we need to know our endianness
	endianness, err := getEndianness()
	if err != nil {
		return nil, err
	}
	h.endian = endianness

	// because syscall package does not provide this
	rall := syscall.RawSockaddrLinklayer{}
	packetRALLSize = int32(unsafe.Sizeof(rall))
	alignedTpacketHdrSize = tpacketAlign(syscall.SizeofTpacket3Hdr)
	alignedTpacketRALLSize = tpacketAlign(packetRALLSize)
	alignedTpacketAllHdrSize = alignedTpacketHdrSize + alignedTpacketRALLSize

	// set up the socket - remember to switch to network socket order for the protocol int
	fd, err := syscall.Socket(syscall.AF_PACKET, syscall.SOCK_RAW, int(htons(syscall.ETH_P_ALL)))
	if err != nil {
		logger.Errorf("failed opening raw socket: %v", err)
		return nil, fmt.Errorf("failed opening raw socket: %v", err)
	}
	h.fd = fd
	h.pollfd = []syscall.PollFd{{Fd: int32(h.fd), Events: syscall.POLLIN}}
	if err := syscall.SetNonblock(fd, false); err != nil {
		return nil, fmt.Errorf("failed to set socket as blocking: %v", err)
	}
	if iface != "" {
		// get our interface
		in, err := net.InterfaceByName(iface)
		if err != nil {
			logger.Errorf("unknown interface %s: %v", iface, err)
			return nil, fmt.Errorf("unknown interface %s: %v", iface, err)
		}
		h.index = in.Index

		// create the sockaddr_ll
		sa := syscall.SockaddrLinklayer{
			Protocol: htons(syscall.ETH_P_ALL),
			Ifindex:  in.Index,
		}
		// bind to it
		if err = syscall.Bind(fd, &sa); err != nil {
			return nil, fmt.Errorf("failed to bind")
		}
		if promiscuous {
			h.promiscuous = true
			mreq := syscall.PacketMreq{
				Ifindex: int32(in.Index),
				Type:    syscall.PACKET_MR_PROMISC,
			}
			if err = syscall.SetsockoptPacketMreq(fd, syscall.SOL_PACKET, syscall.PACKET_ADD_MEMBERSHIP, &mreq); err != nil {
				logger.Errorf("failed to set promiscuous for %s: %v", iface, err)
				return nil, fmt.Errorf("failed to set promiscuous for %s: %v", iface, err)
			}
		}
	}
	if !syscalls {
		if err = syscall.SetsockoptInt(fd, syscall.SOL_PACKET, syscall.PACKET_VERSION, syscall.TPACKET_V3); err != nil {
			logger.Errorf("failed to set TPACKET_V3: %v", err)
			return nil, fmt.Errorf("failed to set TPACKET_V3: %v", err)
		}
		// set up the ring
		var (
			frameSize           = uint32(tpacketAlign(syscall.SizeofTpacket3Hdr+EthHlen) + tpacketAlign(snaplen))
			pageSize            = syscall.Getpagesize()
			blockSize           = uint32(pageSize)
			blockNumbers uint32 = defaultBlockNumbers
		)
		for {
			if blockSize > frameSize {
				break
			}
			blockSize = blockSize << 1
		}
		// we use the default - for now

		framesPerBuffer := blockSize / frameSize
		frameNumbers := blockNumbers * framesPerBuffer

		tpreq := syscall.TpacketReq3{
			Block_size: blockSize,
			Block_nr:   blockNumbers,
			Frame_size: frameSize,
			Frame_nr:   frameNumbers,
		}
		logger.Debugf("creating mmap buffer with tpreq %#v", tpreq)
		if err = syscall.SetsockoptTpacketReq3(fd, syscall.SOL_PACKET, syscall.PACKET_RX_RING, &tpreq); err != nil {
			logger.Errorf("failed to set tpacket req: %v", err)
			return nil, fmt.Errorf("failed to set tpacket req: %v", err)
		}
		totalSize := int(tpreq.Block_size * tpreq.Block_nr)
		data, err := syscall.Mmap(fd, 0, totalSize, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
		if err != nil {
			logger.Errorf("error mmapping: %v", err)
			return nil, fmt.Errorf("error mmapping: %v", err)
		}
		logger.Infof("mmap buffer created with size %d", len(data))
		h.framesPerBuffer = framesPerBuffer
		h.blockSize = int(blockSize)
		h.frameSize = frameSize
		h.frameNumbers = frameNumbers
		h.blockNumbers = int(blockNumbers)
		h.ring = data
		h.cache = make([]captured, 0, blockSize/frameSize)
	}
	return &h, nil
}

// parseSocketAddrLinkLayer parse byte data to get a RawSockAddrLinkLayer
func parseSocketAddrLinkLayer(b []byte, endian binary.ByteOrder) (*syscall.RawSockaddrLinklayer, error) {
	if len(b) < int(packetRALLSize) {
		return nil, fmt.Errorf("bytes of length %d shorter than mandated %d", len(b), packetRALLSize)
	}
	var addr [8]byte
	copy(addr[:], b[11:19])
	sall := syscall.RawSockaddrLinklayer{
		Family:   endian.Uint16(b[0:2]),
		Protocol: endian.Uint16(b[2:4]),
		Ifindex:  int32(endian.Uint32(b[4:8])),
		Hatype:   endian.Uint16(b[8:10]),
		Pkttype:  b[10],
		Halen:    b[11],
		Addr:     addr,
	}
	return &sall, nil
}
