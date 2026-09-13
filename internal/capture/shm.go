package capture

import (
	"fmt"
	"sync"

	"github.com/bnema/wlturbo/wl"
	"golang.org/x/sys/unix"
)

// wl_shm opcodes, from wayland.xml. wlvision talks to wl_shm directly because
// the transport deliberately ships no core-protocol bindings beyond the
// objects it needs.
const (
	shmCreatePoolOpcode    = 0
	poolCreateBufferOpcode = 0
	poolDestroyOpcode      = 1
	bufferDestroyOpcode    = 0
)

// Shm owns the compositor's wl_shm global and hands out mapped buffers for
// captures.
type Shm struct {
	ctx *wl.Context
	shm *wl.BaseProxy

	mu      sync.Mutex
	buffers []*Buffer
}

// BindShm binds wl_shm from the registry.
//
// The caller keeps ownership of the connection: closing a Shm releases its
// buffers, never the display.
func BindShm(ctx *wl.Context, registry *wl.Registry) (*Shm, error) {
	if ctx == nil || registry == nil {
		return nil, fmt.Errorf("bind wl_shm: nil context or registry")
	}

	global, ok := registry.FindGlobal("wl_shm")
	if !ok {
		return nil, fmt.Errorf("compositor does not offer wl_shm")
	}

	shm := &wl.BaseProxy{}
	if err := registry.Bind(global.Name, "wl_shm", global.Version, shm); err != nil {
		return nil, fmt.Errorf("bind wl_shm: %w", err)
	}

	return &Shm{ctx: ctx, shm: shm}, nil
}

// NewBuffer allocates a mapped shared-memory buffer sized for one capture of
// width x height in the given format.
//
// The buffer is registered with the compositor as a wl_buffer, so it can be
// handed to a capture request. Rows are tightly packed: four bytes per pixel.
func (s *Shm) NewBuffer(format Format, width, height int) (*Buffer, error) {
	if width <= 0 || height <= 0 {
		return nil, fmt.Errorf("invalid capture size %dx%d", width, height)
	}
	if format.DRMCode() == 0 {
		return nil, fmt.Errorf("unsupported pixel format %s", format)
	}

	stride := width * 4
	size := stride * height

	fd, err := wl.CreateAnonymousFile(int64(size))
	if err != nil {
		return nil, fmt.Errorf("create capture buffer file: %w", err)
	}

	data, err := wl.MapMemory(fd, size)
	if err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("map capture buffer: %w", err)
	}

	buffer := &Buffer{
		shm:    s,
		fd:     fd,
		data:   data,
		width:  width,
		height: height,
		stride: stride,
		format: format,
	}

	pool := &wl.BaseProxy{}
	pool.SetContext(s.ctx)
	pool.SetID(s.ctx.AllocateID())
	s.ctx.Register(pool)

	// wl_shm.create_pool(new_id pool, fd, size): the descriptor travels out of
	// band, so the body carries the new object and the size only.
	if err := s.ctx.SendRequestWithFDs(s.shm, shmCreatePoolOpcode, []int{fd}, pool, uint32(size)); err != nil {
		_ = unix.Munmap(data)
		_ = unix.Close(fd)
		s.ctx.Unregister(pool)
		return nil, fmt.Errorf("create wl_shm pool: %w", err)
	}

	wlBuffer := &wl.BaseProxy{}
	wlBuffer.SetContext(s.ctx)
	wlBuffer.SetID(s.ctx.AllocateID())
	s.ctx.Register(wlBuffer)

	// wl_shm_pool.create_buffer(new_id buffer, offset, width, height, stride, format)
	if err := s.ctx.SendRequest(pool, poolCreateBufferOpcode,
		wlBuffer, uint32(0), uint32(width), uint32(height), uint32(stride), uint32(format)); err != nil {
		_ = unix.Munmap(data)
		_ = unix.Close(fd)
		s.ctx.Unregister(pool)
		s.ctx.Unregister(wlBuffer)
		return nil, fmt.Errorf("create wl_buffer: %w", err)
	}

	buffer.pool = pool
	buffer.buffer = wlBuffer

	s.mu.Lock()
	s.buffers = append(s.buffers, buffer)
	s.mu.Unlock()

	return buffer, nil
}

// Close releases every buffer this Shm handed out.
func (s *Shm) Close() error {
	s.mu.Lock()
	buffers := s.buffers
	s.buffers = nil
	s.mu.Unlock()

	var firstErr error
	for _, buffer := range buffers {
		if err := buffer.Release(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Buffer is a mapped wl_shm buffer the compositor writes a capture into.
type Buffer struct {
	shm    *Shm
	pool   *wl.BaseProxy
	buffer *wl.BaseProxy

	fd     int
	data   []byte
	width  int
	height int
	stride int
	format Format

	mu       sync.Mutex
	released bool
}

// Pixels exposes the mapped bytes for reading after a capture completes.
//
// The slice aliases the mapping and stays valid until Release. Reading it while
// the compositor is writing is a race the caller owns: a capture is only
// complete after the protocol's complete event.
func (b *Buffer) Pixels() []byte { return b.data }

// Width is the pixel width this buffer was allocated for.
func (b *Buffer) Width() int { return b.width }

// Height is the pixel height this buffer was allocated for.
func (b *Buffer) Height() int { return b.height }

// Stride is the row stride of the mapping, in bytes.
func (b *Buffer) Stride() int { return b.stride }

// Format is the pixel format the buffer was created with.
func (b *Buffer) Format() Format { return b.format }

// Proxy is the wl_buffer to pass to a capture request.
func (b *Buffer) Proxy() *wl.BaseProxy { return b.buffer }

// Release destroys the buffer and its pool, unmaps the memory and closes the
// file descriptor. It is safe to call more than once.
func (b *Buffer) Release() error {
	b.mu.Lock()
	if b.released {
		b.mu.Unlock()
		return nil
	}
	b.released = true
	b.mu.Unlock()

	var firstErr error

	if b.buffer != nil {
		if err := b.shm.ctx.SendRequest(b.buffer, bufferDestroyOpcode); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("destroy wl_buffer: %w", err)
		}
		b.shm.ctx.Unregister(b.buffer)
	}
	if b.pool != nil {
		if err := b.shm.ctx.SendRequest(b.pool, poolDestroyOpcode); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("destroy wl_shm pool: %w", err)
		}
		b.shm.ctx.Unregister(b.pool)
	}
	if b.data != nil {
		if err := wl.UnmapMemory(b.data); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("unmap capture buffer: %w", err)
		}
		b.data = nil
	}
	if b.fd >= 0 {
		if err := unix.Close(b.fd); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("close capture buffer file: %w", err)
		}
		b.fd = -1
	}

	return firstErr
}
