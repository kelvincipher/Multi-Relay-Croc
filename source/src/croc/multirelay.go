package croc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sync"
	"sync/atomic"
	"time"

	"github.com/schollz/croc/v11/src/comm"
	"github.com/schollz/croc/v11/src/crypt"
	"github.com/schollz/croc/v11/src/message"
	"github.com/schollz/croc/v11/src/models"
	"github.com/schollz/croc/v11/src/multirelay"
	"github.com/schollz/croc/v11/src/tcp"
	"github.com/schollz/croc/v11/src/utils"
)

const multiRelayMessage message.Type = "multirelay-agreement-v1"
const multiRelayChunk = int64(models.TCP_BUFFER_SIZE / 2)

var errMultiRelayProtocol = errors.New("multirelay protocol rejected")

type multiRelayState struct {
	config       multirelay.Config
	fingerprint  string
	attempt      int
	agreed       bool // owned by the control loop, reset only after joining workers
	active       atomic.Bool
	activity     atomic.Int64
	setupTimeout time.Duration
	idleTimeout  time.Duration
	mu           sync.Mutex
	verified     map[string]map[int64][32]byte
	manifest     string
	onSend       func(attempt, lane int, position int64) // deterministic test observation
	onAccept     func(attempt, lane int, position int64)
}

// Small nonempty files need ciphertext fragmentation to satisfy separation.
// This version explicitly rejects one-chunk files before any payload is sent.
func validateMultiRelayFiles(files []FileInfo) error {
	for _, file := range files {
		if file.Size < 0 {
			return errors.New("multirelay: invalid file size")
		}
		if file.Symlink != "" {
			return errors.New("multirelay: symlink transfers are not supported")
		}
		if file.Size > 0 && file.Size <= multiRelayChunk {
			return fmt.Errorf("multirelay: nonempty files must exceed %d bytes to occupy both relay lanes", multiRelayChunk)
		}
	}
	return nil
}

func (state *multiRelayState) acceptManifest(files []FileInfo) error {
	encoded, err := json.Marshal(files)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(encoded)
	identity := hex.EncodeToString(digest[:])
	if state.manifest != "" && state.manifest != identity {
		return fmt.Errorf("%w: file manifest changed during recovery", errMultiRelayProtocol)
	}
	state.manifest = identity
	return nil
}

func (c *Client) multiRelayAgreement() multirelay.Agreement {
	pair, _ := c.multi.config.Pair(c.multi.attempt)
	transfer := sha256.Sum256([]byte("croc/multirelay/logical/v1/" + c.baseRoomName))
	return multirelay.Agreement{Version: multirelay.Version, Fingerprint: c.multi.fingerprint, Transfer: hex.EncodeToString(transfer[:]), Pair: pair.ID, Attempt: c.multi.attempt, Sender: c.Options.IsSender}
}
func (c *Client) sendMultiRelayAgreement() error {
	data, err := json.Marshal(c.multiRelayAgreement())
	if err != nil {
		return err
	}
	return message.Send(c.connection(0), c.Key, message.Message{Type: multiRelayMessage, Bytes: data})
}
func (c *Client) processMultiRelayAgreement(m message.Message, attempt *transferAttemptState) error {
	if c.multi == nil || c.multi.agreed || c.Key == nil {
		return fmt.Errorf("%w: unexpected agreement", errMultiRelayProtocol)
	}
	var peer multirelay.Agreement
	decoder := json.NewDecoder(bytes.NewReader(m.Bytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&peer); err != nil {
		return fmt.Errorf("%w: invalid agreement", errMultiRelayProtocol)
	}
	if err := c.multiRelayAgreement().Validate(peer); err != nil {
		return fmt.Errorf("%w: %v", errMultiRelayProtocol, err)
	}
	c.multi.agreed = true
	if err := c.openMultiRelayData(attempt); err != nil {
		return err
	}
	c.multi.activity.Store(time.Now().UnixNano())
	c.multi.active.Store(true)
	c.selectedDataTransport.Store(selectedTransportRelay)
	attempt.finishTailcatSetup()
	return c.finishDataTransportActivation()
}

func (c *Client) openMultiRelayData(attempt *transferAttemptState) error {
	pair, err := c.multi.config.Pair(c.multi.attempt)
	if err != nil {
		return err
	}
	var connections [2]*comm.Comm
	var failures [2]error
	var wg sync.WaitGroup
	ctx, cancel := context.WithTimeout(attempt.ctx, c.multi.setupDuration())
	defer cancel()
	for lane, relay := range pair.Relays {
		wg.Add(1)
		go func(lane int, relay multirelay.Relay) {
			defer wg.Done()
			password, err := relay.Password(c.Options.RelayPassword)
			if err != nil {
				failures[lane] = err
				return
			}
			// Legacy admission is supported on each data port. A capability issued by
			// relay A must never be presented as authorization to relay B.
			room := fmt.Sprintf("%s-mr-v1-%d", c.Options.RoomName, lane)
			connection, _, _, _, err := tcp.ConnectToTCPServerWithCapabilityContext(ctx, relay.DataAddress(), password, room, "", c.multi.setupDuration())
			connections[lane], failures[lane] = connection, err
			if err != nil {
				cancel()
			}
		}(lane, relay)
	}
	wg.Wait()
	if failures[0] != nil || failures[1] != nil {
		for _, connection := range connections {
			if connection != nil {
				connection.Close()
			}
		}
		return fmt.Errorf("%w: both multirelay data connections are required", ErrRelayConnection)
	}
	// Install both before admitting workers; worker index is the immutable lane.
	c.setConnection(1, connections[0])
	c.setConnection(2, connections[1])
	if !c.Options.IsSender {
		for lane, connection := range connections {
			c.startReceiveData(lane, connection, attempt)
		}
	}
	return nil
}
func (state *multiRelayState) setupDuration() time.Duration {
	if state.setupTimeout > 0 {
		return state.setupTimeout
	}
	return 120 * time.Second
}
func (state *multiRelayState) idleDuration() time.Duration {
	if state.idleTimeout > 0 {
		return state.idleTimeout
	}
	return 45 * time.Second
}

// Every attempt owns one disjoint pair, one fresh PAKE exchange and its own
// watchdog. Neither peer accepts application data until its encrypted agreement
// matches. Deterministic room names let independently detected failures meet on
// the next pair without trusting retry instructions from a relay.
func (c *Client) multiRelayTransfer() error {
	var lastErr error
	for index := range c.multi.config.Pairs {
		if err := c.ctxErr(); err != nil {
			return err
		}
		c.multi.attempt, c.multi.agreed = index, false
		c.multi.active.Store(false)
		if index > 0 {
			room := sha256.Sum256([]byte(fmt.Sprintf("croc/multirelay/attempt/v1/%s/%d", c.baseRoomName, index)))
			c.nextReconnectRoom = hex.EncodeToString(room[:])
			if err := c.resetForReconnectAttempt(index); err != nil {
				return err
			}
		}
		pair, _ := c.multi.config.Pair(index)
		if index > 0 {
			fmt.Fprintf(os.Stderr, "\nMulti-Relay: reconnecting with unused pair %s\n", pair.ID)
		}
		password, err := pair.Relays[0].Password(c.Options.RelayPassword)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(c.stop.ctx, c.multi.setupDuration())
		connection, _, externalIP, capability, err := tcp.ConnectToTCPServerControlContext(ctx, pair.Relays[0].ControlAddress(), password, c.Options.RoomName, c.multi.setupDuration())
		if err != nil {
			cancel()
			lastErr = err
			continue
		}
		closeOnCancel := context.AfterFunc(ctx, connection.Close)
		if c.Options.IsSender {
			err = c.senderWaitForHandshake(connection)
		} else {
			err = connection.Send(handshakeRequest)
		}
		stopped := closeOnCancel()
		cancel()
		if err != nil || !stopped {
			connection.Close()
			lastErr = errors.New("multirelay peer rendezvous failed or timed out")
			continue
		}
		c.setConnection(0, connection)
		c.setRelayControlRoute(pair.Relays[0].ControlAddress(), capability, false)
		c.ExternalIP = externalIP
		c.markExternalIPReady()
		c.markSenderRouteReady()
		watchCtx, stopWatch := context.WithCancel(c.stop.ctx)
		watchDone := make(chan struct{})
		go func() { defer close(watchDone); c.watchMultiRelayAttempt(watchCtx, connection) }()
		err = c.transfer()
		stopWatch()
		<-watchDone
		c.closeAttempt()
		if err == nil {
			return nil
		}
		if c.ctxErr() != nil {
			return c.ctxErr()
		}
		if errors.Is(err, errMultiRelayProtocol) {
			return err
		}
		// Authentication, integrity, disk and peer refusal errors are terminal.
		// Network failures can advance only to the next unused pair.
		if !isTransferDisconnectError(err) && !errors.Is(err, ErrRelayConnection) && c.multi.agreed {
			return err
		}
		lastErr = err
	}
	return fmt.Errorf("%w: transfer could not complete (%v)", multirelay.ErrExhausted, lastErr)
}
func (c *Client) watchMultiRelayAttempt(ctx context.Context, connection *comm.Comm) {
	start := time.Now()
	interval := min(time.Second, c.multi.idleDuration()/4)
	if interval < time.Millisecond {
		interval = time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if !c.multi.active.Load() || !c.activeTransferStarted() {
				if now.Sub(start) > c.multi.setupDuration() {
					connection.Close()
					return
				}
			} else if c.activeTransferStarted() && now.Sub(time.Unix(0, c.multi.activity.Load())) > c.multi.idleDuration() {
				connection.Close()
				return
			}
		}
	}
}

func (queue *requestedChunkQueue) claimForLane(lane int) (int64, bool) {
	if !queue.striped {
		return queue.claim()
	}
	if lane < 0 || lane > 1 {
		return 0, false
	}
	queue.mu.Lock()
	defer queue.mu.Unlock()
	for queue.laneNext[lane]+1 < len(queue.ranges) {
		cursor := queue.laneNext[lane]
		for queue.laneStep[lane] < queue.ranges[cursor+1] {
			offset := queue.ranges[cursor] + queue.laneStep[lane]*queue.ranges[0]
			queue.laneStep[lane]++
			if (offset/multiRelayChunk)%2 == int64(lane) {
				return offset, true
			}
		}
		queue.laneNext[lane] += 2
		queue.laneStep[lane] = 0
	}
	return 0, false
}
func (c *Client) multiRelayFrameLimit() int {
	if c.multi != nil {
		return maxDecompressedChunkSize + 32
	}
	return maxDecompressedChunkSize
}
func multiRelayFileKey(file FileInfo) string {
	return fmt.Sprintf("%s/%d/%x", path.Join(file.FolderRemote, file.Name), file.Size, file.Hash)
}

// accept serializes duplicate detection with a complete WriteAt. Only bytes
// authenticated by croc's existing AEAD and written in full enter verified.
func (state *multiRelayState) accept(file *os.File, info FileInfo, lane int, position int64, payload []byte, ranges []int64) (bool, error) {
	if position < 0 || position%multiRelayChunk != 0 || int64(lane) != (position/multiRelayChunk)%2 || int64(len(payload)) != min(multiRelayChunk, info.Size-position) || !utils.ChunkRangesContain(ranges, position) {
		return false, fmt.Errorf("%w: invalid data lane, length or range", errMultiRelayProtocol)
	}
	state.mu.Lock()
	key := multiRelayFileKey(info)
	chunks := state.verified[key]
	if chunks == nil {
		chunks = make(map[int64][32]byte)
		state.verified[key] = chunks
	}
	digest := sha256.Sum256(payload)
	if prior, exists := chunks[position]; exists {
		state.mu.Unlock()
		if prior != digest {
			return false, fmt.Errorf("%w: conflicting replay", errMultiRelayProtocol)
		}
		return false, nil
	}
	n, err := file.WriteAt(payload, position)
	if err == nil && n != len(payload) {
		err = io.ErrShortWrite
	}
	if err != nil {
		state.mu.Unlock()
		return false, err
	}
	chunks[position] = digest
	state.mu.Unlock()
	state.activity.Store(time.Now().UnixNano())
	if state.onAccept != nil {
		state.onAccept(state.attempt, lane, position)
	}
	return true, nil
}

// Re-read retained chunks before trusting them after reconnection. Zero-filled
// source chunks are valid data, never confused with sparse holes.
func (state *multiRelayState) missing(file *os.File, info FileInfo) ([]int64, error) {
	state.mu.Lock()
	defer state.mu.Unlock()
	chunks := state.verified[multiRelayFileKey(info)]
	ranges := []int64{multiRelayChunk}
	buffer := make([]byte, multiRelayChunk)
	for offset := int64(0); offset < info.Size; offset += multiRelayChunk {
		valid := false
		if expected, exists := chunks[offset]; exists {
			size := min(multiRelayChunk, info.Size-offset)
			n, err := file.ReadAt(buffer[:size], offset)
			valid = n == int(size) && (err == nil || err == io.EOF) && sha256.Sum256(buffer[:size]) == expected
			if !valid {
				delete(chunks, offset)
			}
		}
		if !valid {
			count := len(ranges)
			if count >= 3 && ranges[count-2]+ranges[count-1]*multiRelayChunk == offset {
				ranges[count-1]++
			} else {
				ranges = append(ranges, offset, 1)
			}
		}
	}
	if len(ranges) == 1 {
		return nil, errors.New("multirelay: no missing ranges, destination must be checked again")
	}
	return ranges, nil
}
func verifyMultiRelayFile(file *os.File, info FileInfo) error {
	if err := file.Sync(); err != nil {
		return err
	}
	digest := sha256.New()
	if _, err := io.Copy(digest, io.NewSectionReader(file, 0, info.Size)); err != nil {
		return err
	}
	if !bytes.Equal(digest.Sum(nil), info.Hash) {
		return fmt.Errorf("%w: final SHA-256 mismatch", errMultiRelayProtocol)
	}
	return nil
}

// Existing croc control encryption has no sequence number. Bind every control
// frame in this mode to its direction and monotonically increasing sequence,
// using the existing AES-GCM implementation with a fresh random nonce per frame.
func (c *Client) secureMultiRelayControl() {
	key := append([]byte(nil), c.Key...)
	role := byte(0)
	if c.Options.IsSender {
		role = 1
	}
	var sent, received uint64
	c.connection(0).SetFrameCodec(func(payload []byte) ([]byte, error) {
		if sent == ^uint64(0) {
			return nil, fmt.Errorf("%w: sequence exhausted", errMultiRelayProtocol)
		}
		frame := make([]byte, 9+len(payload))
		frame[0] = role
		binary.BigEndian.PutUint64(frame[1:9], sent)
		copy(frame[9:], payload)
		encrypted, err := crypt.Encrypt(frame, key)
		if err == nil {
			sent++
		}
		return encrypted, err
	}, func(frame []byte) ([]byte, error) {
		plain, err := crypt.Decrypt(frame, key)
		if err != nil || len(plain) < 9 || plain[0] != 1-role || binary.BigEndian.Uint64(plain[1:9]) != received || received == ^uint64(0) {
			return nil, fmt.Errorf("%w: control authentication, direction or sequence", errMultiRelayProtocol)
		}
		received++
		c.multi.activity.Store(time.Now().UnixNano())
		return plain[9:], nil
	})
}
