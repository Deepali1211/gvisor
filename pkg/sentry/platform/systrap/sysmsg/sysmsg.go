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

// Package sysmsg provides a stub signal handler and a communication protocol
// between stub threads and the Sentry.
//
// Note that this package is allowlisted for use of sync/atomic.
//
// +checkalignedignore
package sysmsg

import (
	"fmt"
	"strings"
	"sync/atomic"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/abi/linux/errno"
	"gvisor.dev/gvisor/pkg/errors"
	"gvisor.dev/gvisor/pkg/hostarch"
)

// LINT.IfChange
// Per-thread stack layout:
//
// *------------*
// | guard page |
// |------------|
// |            |
// |  sysstack  |
// |            |
// *------------*
// | guard page |
// |------------|
// |            |
// |     ^      |
// |    / \     |
// |     |      |
// |  altstack  |
// |------------|
// |   sysmsg   |
// *------------*
const (
	// PerThreadMemSize is the size of a per-thread memory region.
	PerThreadMemSize = 8 * hostarch.PageSize
	// GuardSize is the size of an unmapped region which is placed right
	// before the signal stack.
	GuardSize                   = hostarch.PageSize
	PerThreadPrivateStackOffset = GuardSize
	PerThreadPrivateStackSize   = 2 * hostarch.PageSize
	// PerThreadStackSharedSize is the size of a per-thread stack region.
	PerThreadSharedStackSize   = 4 * hostarch.PageSize
	PerThreadSharedStackOffset = 4 * hostarch.PageSize
	// MsgOffsetFromStack is the offset of the Msg structure on
	// the thread stack.
	MsgOffsetFromSharedStack = PerThreadMemSize - hostarch.PageSize - PerThreadSharedStackOffset
)

// StackAddrToMsg returns an address of a sysmsg structure.
func StackAddrToMsg(sp uintptr) uintptr {
	return sp + MsgOffsetFromSharedStack
}

// StackAddrToSyshandlerStack returns an address of a syshandler stack.
func StackAddrToSyshandlerStack(sp uintptr) uintptr {
	return sp + PerThreadPrivateStackOffset + PerThreadPrivateStackSize
}

// MsgToStackAddr returns a start address of a stack.
func MsgToStackAddr(msg uintptr) uintptr {
	return msg - MsgOffsetFromSharedStack
}

// ThreadState is used to store a state of the sysmsg thread.
type ThreadState uint32

// Set atomicaly sets the state value.
func (s *ThreadState) Set(state ThreadState) {
	atomic.StoreUint32((*uint32)(s), uint32(state))
}

// Get returns the current state value.
//
//go:nosplit
func (s *ThreadState) Get() ThreadState {
	return ThreadState(atomic.LoadUint32((*uint32)(s)))
}

const (
	// ThreadStateNone means that the thread is executing the user workload.
	ThreadStateNone ThreadState = iota
	// ThreadStateDone means that last event has been handled and the stub thread
	// can be resumed.
	ThreadStateDone
	// ThreadStateEvent means that there is a new event which has to be handled by
	// Sentry. (Used only if !contextDecoupledExp)
	ThreadStateEvent
	// ThreadStatePrep means that syshandler started filling the sysmsg struct.
	ThreadStatePrep
	// ThreadStateInterrupt is a Sysmsg state that indicates to the sighandler
	// that there is a postponed interrupt from the syshandler.
	// The sentry should never see this event.
	ThreadStateInterrupt
)

// Msg contains the current state of the sysmsg thread.
type Msg struct {
	// The next batch of fields is used to call the syshandler stub
	// function. A system call can be replaced with a function call. When
	// a function call is executed, it can't change the current process
	// stack, so it needs to save stack and instruction registers, switch
	// on its syshandler stack and call the jmp instruction to the syshandler
	// address.
	//
	// Self is a pointer to itself in a process address space.
	Self uint64
	// RetAddr is a return address from the syshandler function.
	RetAddr uint64
	// Syshandler is an address of the syshandler function.
	Syshandler uint64
	// SyshandlerStack is an address of  the thread syshandler stack.
	SyshandlerStack uint64
	// AppStack is a value of the stack register before calling the syshandler
	// function.
	AppStack uint64
	// interrupt is non-zero if there is a postponed interrupt.
	interrupt uint32
	// State indicates to the sentry what the sysmsg thread is doing at a given
	// moment.
	State ThreadState
	// ContextID is the ID of the ThreadContext struct that the current
	// sysmsg thread is is processing. This ID is used in the {sig|sys}handler
	// to find the offset to the correct ThreadContext struct location.
	ContextID uint64
	// ContextRegion defines the ThreadContext memory region start within
	// the sysmsg thread address space.
	ContextRegion uint64

	// InterruptedContextID is the target of the interrupt sent to sysmsg thread.
	InterruptedContextID uint64
	// FaultJump is the size of a faulted instruction.
	FaultJump int32
	// Err is the error value with which the {sig|sys}handler crashes the stub
	// thread (see sysmsg.h:__panic).
	Err int32
	// Line is the code line on which the {sig|sys}handler crashed the stub thread
	// (see sysmsg.h:panic).
	Line int32
	// Debug is a variable to use to get visibility into the stub from the sentry.
	Debug uint64
	// fpState is an offset relative to the sighandler stack to the fpState, stored
	// by the sighandler.
	fpState uint64
	// TLS is a pointer to a thread local storage.
	// It is is only populated on ARM64.
	TLS uint64
	// The fast path is the mode when a thread is polling msg->state to
	// wait for a required state instead of calling FUTEX_WAIT.
	//
	// If core tagging is supported by the kernel, the Sentry thread, and a
	// stub thread share the same cookie and run on two associated
	// hyper-threads. The thread which is polling msg->state calls the
	// pause instruction, so the second thread gets almost the entire core
	// to run its workload.
	//
	// If core tagging isn't supported, a polling thread calls sched_yield
	// to let other processes to run.

	// StubFastPath is set if a stub process uses the fast path to wait for
	// events.  Only the stub thread can set it before switching to the
	// sentry, but the Sentry can clear it.
	stubFastPath   uint32
	sentryFastPath uint32
	AckedEvents    uint32
}

// ContextState defines the reason the context has exited back to the sentry,
// or ContextStateNone if running/ready-to-run.
type ContextState uint32

// Set atomicaly sets the state value.
func (s *ContextState) Set(state ContextState) {
	atomic.StoreUint32((*uint32)(s), uint32(state))
}

// Get returns the current state value.
//
//go:nosplit
func (s *ContextState) Get() ContextState {
	return ContextState(atomic.LoadUint32((*uint32)(s)))
}

// Context State types.
const (
	// ContextStateNone means that is either running in the user task or is ready
	// to run in the user task.
	ContextStateNone ContextState = iota
	// ContextStateSyscall means that a syscall event is triggered from the
	// sighandler.
	ContextStateSyscall
	// ContextStateFault means that there is a fault event that needs to be
	// handled.
	ContextStateFault
	// ContextStateSyscallTrap means that a syscall event is triggered from
	// a function call (syshandler).
	ContextStateSyscallTrap
	// ContextStateSyscallCanBePatched means that the syscall can be replaced
	// with a function call.
	ContextStateSyscallCanBePatched
	// ContextStateInvalid is an invalid state that the sentry should never see.
	ContextStateInvalid
)

const (
	// MaxFPStateLen is the largest possible FPState that we will save.
	// Note: This value was chosen to be able to fit ThreadContext into one page.
	MaxFPStateLen uint32 = 3648

	// AllocatedSizeofThreadContextStruct defines how much memory to allocate for
	// one instance of ThreadContext.
	// We over allocate the memory for it because:
	//   - The next instances needs to align to 64 bytes for purposes of xsave.
	//   - It's nice to align it to the page boundary.
	AllocatedSizeofThreadContextStruct uintptr = 4096
)

// ThreadContext contains the current context of the sysmsg thread. The struct
// facilitates switching contexts by allowing the sentry to switch pointers to
// this struct as it needs to.
type ThreadContext struct {
	// FPState is a region of memory where:
	//   - syshandler saves FPU state to using xsave/fxsave
	//   - sighandler copies FPU state to from ucontext->uc_mcontext.fpregs
	// Note that xsave requires this region of memory to be 64 byte aligned;
	// therefore allocations of ThreadContext must be too.
	FPState [MaxFPStateLen]byte
	// FPStateChanged is set to true when the stub thread needs to restore FPState
	// because the sentry changed it.
	FPStateChanged uint64
	// Regs is the context's GP register set. The {sig|sys}handler will save and
	// restore the user app's registers here.
	Regs linux.PtraceRegs

	// SignalInfo is the siginfo struct.
	SignalInfo linux.SignalInfo
	// Signo is the signal that the stub is requesting the sentry to handle.
	Signo int64
	// State indicates the reason why the context has exited back to the sentry.
	State ContextState
	// Interrupt is set to indicate that this context has been interrupted.
	Interrupt uint32
	// Debug is a variable to use to get visibility into the stub from the sentry.
	Debug uint64
}

// LINT.ThenChange(sysmsg.h)

// Init initializes the message.
func (m *Msg) Init() {
	m.Err = 0
	m.Line = -1
	m.stubFastPath = 0
	m.sentryFastPath = 1
}

// Init initializes the ThreadContext instance.
func (c *ThreadContext) Init() {
	c.FPStateChanged = 1
	c.Regs = linux.PtraceRegs{}
	c.Signo = 0
	c.SignalInfo = linux.SignalInfo{}
	c.State = ContextStateNone
}

// StubFastPath returns true if the stub thread in the polling mode.
func (m *Msg) StubFastPath() bool {
	return atomic.LoadUint32(&m.stubFastPath) != 0
}

// DisableStubFastPath disables the polling mode for the stub thread.
func (m *Msg) DisableStubFastPath() {
	atomic.StoreUint32(&m.stubFastPath, 0)
}

// EnableSentryFastPath enables the polling mode for the Sentry. It has to be
// called before switching controls to the stub process.
func (m *Msg) EnableSentryFastPath() {
	m.sentryFastPath = 1
}

// DisableSentryFastPath disables the polling mode for the Sentry.
func (m *Msg) DisableSentryFastPath() {
	atomic.StoreUint32(&m.sentryFastPath, 0)
}

// FPUStateOffset returns the offset of a saved FPU state to the msg.
func (m *Msg) FPUStateOffset() (uint64, error) {
	offset := m.fpState
	if int64(offset) > -MsgOffsetFromSharedStack && int64(offset) < 0 {
		return offset, nil
	}
	return 0, errors.New(errno.EFAULT, fmt.Sprintf("FPU offset has been corrupted: %x", offset))
}

func (m *Msg) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "sysmsg.Msg{msg: %x state %d", m.Self, m.State)
	fmt.Fprintf(&b, " err %x line %d debug %x", m.Err, m.Line, m.Debug)
	fmt.Fprintf(&b, " app stack %x", m.AppStack)
	fmt.Fprintf(&b, " contextID %d", m.ContextID)
	b.WriteString("}")

	return b.String()
}

func (c *ThreadContext) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "sysmsg.ThreadContext{state %d", c.State.Get())
	fmt.Fprintf(&b, " fault addr %x syscall %d", c.SignalInfo.Addr(), c.SignalInfo.Syscall())
	fmt.Fprintf(&b, " ip %x sp %x", c.Regs.InstructionPointer(), c.Regs.StackPointer())
	fmt.Fprintf(&b, " FPStateChanged %d Regs %+v", c.FPStateChanged, c.Regs)
	fmt.Fprintf(&b, " signo: %d, siginfo: %+v", c.Signo, c.SignalInfo)
	fmt.Fprintf(&b, " debug %d", atomic.LoadUint64(&c.Debug))
	b.WriteString("}")

	return b.String()
}