// Copyright 2020 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package udp_icmp_error_propagation_test

import (
	"context"
	"fmt"
	"net"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	tb "gvisor.dev/gvisor/test/packetimpact/testbench"
)

type connectionMode bool

func (c connectionMode) String() string {
	if c {
		return "Connected"
	}
	return "Connectionless"
}

type icmpError int

const (
	portUnreachable icmpError = iota
	timeToLiveExceeded
)

func (e icmpError) String() string {
	switch e {
	case portUnreachable:
		return "PortUnreachable"
	case timeToLiveExceeded:
		return "TimeToLiveExpired"
	}
	return "Unknown ICMP error"
}

func (e icmpError) ToICMPv4() *tb.ICMPv4 {
	switch e {
	case portUnreachable:
		return &tb.ICMPv4{Type: tb.ICMPv4Type(header.ICMPv4DstUnreachable), Code: tb.Uint8(header.ICMPv4PortUnreachable)}
	case timeToLiveExceeded:
		return &tb.ICMPv4{Type: tb.ICMPv4Type(header.ICMPv4TimeExceeded), Code: tb.Uint8(header.ICMPv4TTLExceeded)}
	}
	return nil
}

type errorDetection struct {
	name         string
	useValidConn bool
	f            func(context.Context, testData) error
}

type testData struct {
	dut        *tb.DUT
	conn       *tb.UDPIPv4
	remoteFD   int32
	remotePort uint16
	cleanFD    int32
	cleanPort  uint16
	wantErrno  syscall.Errno
}

// testRecv tests observing the ICMP error through the recv syscall. A packet
// is sent to the DUT, and if wantErrno is non-zero, then the first recv should
// fail and the second should succeed. Otherwise if wantErrno is zero then the
// first recv should succeed immediately.
func testRecv(ctx context.Context, d testData) error {
	// Check that receiving on the clean socket works.
	d.conn.Send(tb.UDP{DstPort: &d.cleanPort})
	d.dut.Recv(d.cleanFD, 100, 0)

	d.conn.Send(tb.UDP{})

	if d.wantErrno != syscall.Errno(0) {
		ctx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		ret, _, err := d.dut.RecvWithErrno(ctx, d.remoteFD, 100, 0)
		if ret != -1 {
			return fmt.Errorf("recv after ICMP error succeeded unexpectedly, expected (%[1]d) %[1]v", d.wantErrno)
		}
		if err != d.wantErrno {
			return fmt.Errorf("recv after ICMP error resulted in error (%[1]d) %[1]v, expected (%[2]d) %[2]v", err, d.wantErrno)
		}
	}

	d.dut.Recv(d.remoteFD, 100, 0)
	return nil
}

// testSendTo tests observing the ICMP error through the send syscall. If
// wantErrno is non-zero, the first send should fail and a subsequent send
// should suceed; while if wantErrno is zero then the first send should just
// succeed.
func testSendTo(ctx context.Context, d testData) error {
	// Check that sending on the clean socket works.
	d.dut.SendTo(d.cleanFD, nil, 0, d.conn.LocalAddr())
	if _, err := d.conn.Expect(tb.UDP{SrcPort: &d.cleanPort}, time.Second); err != nil {
		return fmt.Errorf("did not receive UDP packet from clean socket on DUT: %s", err)
	}

	if d.wantErrno != syscall.Errno(0) {
		ctx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		ret, err := d.dut.SendToWithErrno(ctx, d.remoteFD, nil, 0, d.conn.LocalAddr())

		if ret != -1 {
			return fmt.Errorf("sendto after ICMP error succeeded unexpectedly, expected (%[1]d) %[1]v", d.wantErrno)
		}
		if err != d.wantErrno {
			return fmt.Errorf("sendto after ICMP error resulted in error (%[1]d) %[1]v, expected (%[2]d) %[2]v", err, d.wantErrno)
		}
	}

	d.dut.SendTo(d.remoteFD, nil, 0, d.conn.LocalAddr())
	if _, err := d.conn.Expect(tb.UDP{}, time.Second); err != nil {
		return fmt.Errorf("did not receive UDP packet as expected: %s", err)
	}
	return nil
}

func testSockOpt(_ context.Context, d testData) error {
	// Check that there's no pending error on the clean socket.
	if errno := syscall.Errno(d.dut.GetSockOptInt(d.cleanFD, unix.SOL_SOCKET, unix.SO_ERROR)); errno != syscall.Errno(0) {
		return fmt.Errorf("unexpected error (%[1]d) %[1]v on clean socket", errno)
	}

	if errno := syscall.Errno(d.dut.GetSockOptInt(d.remoteFD, unix.SOL_SOCKET, unix.SO_ERROR)); errno != d.wantErrno {
		return fmt.Errorf("SO_ERROR sockopt after ICMP error is (%[1]d) %[1]v, expected (%[2]d) %[2]v", errno, d.wantErrno)
	}

	// Check that after clearing socket error, sending doesn't fail.
	d.dut.SendTo(d.remoteFD, nil, 0, d.conn.LocalAddr())
	if _, err := d.conn.Expect(tb.UDP{}, time.Second); err != nil {
		return fmt.Errorf("did not receive UDP packet as expected: %s", err)
	}
	return nil
}

// TestUDPICMPErrorPropagation tests that ICMP error messages in response to
// UDP datagrams are processed correctly. RFC 1122 section 4.1.3.3 states that:
// "UDP MUST pass to the application layer all ICMP error messages that it
// receives from the IP layer."
//
// The test cases are parametrized in 3 dimensions: 1. the UDP socket is either
// put into connection mode or left connectionless, 2. the ICMP message type
// and code, and 3. the method by which the ICMP error is observed on the
// socket: sendto, recv, or getsockopt(SO_ERROR).
//
// Linux's udp(7) man page states: "All fatal errors will be passed to the user
// as an error return even when the socket is not connected. This includes
// asynchronous errors received from the network." In practice, the only
// combination of parameters to the test that causes an error to be observable
// on the UDP socket is receiving a port unreachable message on a connected
// socket.
func TestUDPICMPErrorPropagation(t *testing.T) {
	for _, connect := range []connectionMode{true, false} {
		for _, icmpErr := range []icmpError{portUnreachable, timeToLiveExceeded} {
			wantErrno := syscall.Errno(0)
			if connect && icmpErr == portUnreachable {
				wantErrno = unix.ECONNREFUSED
			}
			for _, errDetect := range []errorDetection{
				errorDetection{"SendTo", false, testSendTo},
				// Send to an address that's different from the one that caused an ICMP
				// error to be returned.
				errorDetection{"SendToValid", true, testSendTo},
				errorDetection{"Recv", false, testRecv},
				errorDetection{"SockOpt", false, testSockOpt},
			} {
				t.Run(fmt.Sprintf("%s/%s/%s", connect, icmpErr, errDetect.name), func(t *testing.T) {
					dut := tb.NewDUT(t)
					defer dut.TearDown()

					remoteFD, remotePort := dut.CreateBoundSocket(unix.SOCK_DGRAM, unix.IPPROTO_UDP, net.ParseIP("0.0.0.0"))
					defer dut.Close(remoteFD)

					// Create a second, clean socket on the DUT to ensure that the ICMP
					// error messages only affect the sockets they are intended for.
					cleanFD, cleanPort := dut.CreateBoundSocket(unix.SOCK_DGRAM, unix.IPPROTO_UDP, net.ParseIP("0.0.0.0"))
					defer dut.Close(cleanFD)

					conn := tb.NewUDPIPv4(t, tb.UDP{DstPort: &remotePort}, tb.UDP{SrcPort: &remotePort})
					defer conn.Close()

					if connect {
						dut.Connect(remoteFD, conn.LocalAddr())
						dut.Connect(cleanFD, conn.LocalAddr())
					}

					dut.SendTo(remoteFD, nil, 0, conn.LocalAddr())
					udp, err := conn.Expect(tb.UDP{}, time.Second)
					if err != nil {
						t.Fatalf("did not receive message from DUT: %s", err)
					}

					if icmpErr == timeToLiveExceeded {
						ip, ok := udp.Prev().(*tb.IPv4)
						if !ok {
							t.Fatalf("expected %s to be IPv4", udp.Prev())
						}
						*ip.TTL = 1
						// Let serialization recalculate the checksum since we set the TTL
						// to 1.
						ip.Checksum = nil

						// Note that the ICMP payload is valid in this case because the UDP
						// payload is empty. If the UDP payload were not empty, the packet
						// length during serialization may not be calculated correctly,
						// resulting in a mal-formed packet.
						conn.SendIP(icmpErr.ToICMPv4(), ip, udp)
					} else {
						conn.SendIP(icmpErr.ToICMPv4(), udp.Prev(), udp)
					}

					errDetectConn := &conn
					if errDetect.useValidConn {
						// connClean is a UDP socket on the test runner that was not
						// involved in the generation of the ICMP error. As such,
						// interactions between it and the the DUT should be independent of
						// the ICMP error at least at the port level.
						connClean := tb.NewUDPIPv4(t, tb.UDP{DstPort: &remotePort}, tb.UDP{SrcPort: &remotePort})
						defer connClean.Close()

						errDetectConn = &connClean
					}

					if err := errDetect.f(context.Background(), testData{&dut, errDetectConn, remoteFD, remotePort, cleanFD, cleanPort, wantErrno}); err != nil {
						t.Fatal(err)
					}
				})
			}
		}
	}
}
