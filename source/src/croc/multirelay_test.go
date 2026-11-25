package croc

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/schollz/croc/v11/src/comm"
	"github.com/schollz/croc/v11/src/crypt"
	"github.com/schollz/croc/v11/src/multirelay"
	"github.com/schollz/croc/v11/src/tcp"
)

func TestMultiRelayLaneQueue(t *testing.T) {
	for _, ranges := range [][]int64{nil, {multiRelayChunk, 0, 2, 4 * multiRelayChunk, 3}} {
		q := newRequestedChunkQueue(ranges, 9*multiRelayChunk, multiRelayChunk, nil)
		q.striped = true
		q.laneNext = [2]int{1, 1}
		seen := sync.Map{}
		var wg sync.WaitGroup
		for lane := 0; lane < 2; lane++ {
			wg.Add(1)
			go func(lane int) {
				defer wg.Done()
				for {
					offset, ok := q.claimForLane(lane)
					if !ok {
						return
					}
					if int(offset/multiRelayChunk)%2 != lane {
						t.Errorf("wrong lane %d offset %d", lane, offset)
					}
					if _, exists := seen.LoadOrStore(offset, true); exists {
						t.Error("duplicate chunk")
					}
				}
			}(lane)
		}
		wg.Wait()
		count := 0
		seen.Range(func(_, _ any) bool { count++; return true })
		if count != q.total {
			t.Fatalf("got %d chunks want %d", count, q.total)
		}
	}
}
func TestMultiRelayVerifiedRangesAndReconstruction(t *testing.T) {
	contents := make([]byte, 3*multiRelayChunk+17) // all-zero data is deliberately valid
	hash := sha256.Sum256(contents)
	info := FileInfo{Name: "data", Size: int64(len(contents)), Hash: hash[:]}
	file, err := os.CreateTemp(t.TempDir(), "data")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err = file.Truncate(info.Size); err != nil {
		t.Fatal(err)
	}
	state := &multiRelayState{verified: map[string]map[int64][32]byte{}}
	for _, index := range []int64{3, 1, 0, 2} {
		start := index * multiRelayChunk
		end := min(start+multiRelayChunk, info.Size)
		accepted, err := state.accept(file, info, int(index%2), start, contents[start:end], nil)
		if err != nil || !accepted {
			t.Fatal(accepted, err)
		}
		accepted, err = state.accept(file, info, int(index%2), start, contents[start:end], nil)
		if err != nil || accepted {
			t.Fatal("replay should be ignored", accepted, err)
		}
	}
	if err = verifyMultiRelayFile(file, info); err != nil {
		t.Fatal(err)
	}
	// A disk change invalidates exactly that retained chunk.
	if _, err = file.WriteAt([]byte{1}, multiRelayChunk); err != nil {
		t.Fatal(err)
	}
	missing, err := state.missing(file, info)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(missing) != fmt.Sprint([]int64{multiRelayChunk, multiRelayChunk, 1}) {
		t.Fatal(missing)
	}
	if _, err = state.accept(file, info, 0, multiRelayChunk, contents[multiRelayChunk:2*multiRelayChunk], missing); err == nil {
		t.Fatal("wrong parity accepted")
	}
	if _, err = state.accept(file, info, 1, multiRelayChunk, contents[multiRelayChunk:2*multiRelayChunk], missing); err != nil {
		t.Fatal(err)
	}
	if err = verifyMultiRelayFile(file, info); err != nil {
		t.Fatal(err)
	}
}
func TestMultiRelaySmallFilesFailClosed(t *testing.T) {
	for _, size := range []int64{1, 12, multiRelayChunk} {
		if validateMultiRelayFiles([]FileInfo{{Size: size}}) == nil {
			t.Fatal("one-chunk file accepted")
		}
	}
	for _, size := range []int64{0, multiRelayChunk + 1, 4 * multiRelayChunk} {
		if err := validateMultiRelayFiles([]FileInfo{{Size: size}}); err != nil {
			t.Fatal(err)
		}
	}
}

// Recording the server-facing network stream observes the ciphertext actually
// reaching a relay, independently of client routing hooks.
type mrRecordConn struct {
	net.Conn
	mu       sync.Mutex
	data     []byte
	writeMu  sync.Mutex
	pending  []byte
	fault    string
	gate     <-chan struct{}
	injected bool
}

func (conn *mrRecordConn) Read(buffer []byte) (int, error) {
	n, err := conn.Conn.Read(buffer)
	conn.mu.Lock()
	conn.data = append(conn.data, buffer[:n]...)
	conn.mu.Unlock()
	return n, err
}

type mrRecordListener struct {
	net.Listener
	mu          sync.Mutex
	connections []*mrRecordConn
	fault       string
	gate        <-chan struct{}
}

func (listener *mrRecordListener) Accept() (net.Conn, error) {
	connection, err := listener.Listener.Accept()
	if err != nil {
		return nil, err
	}
	listener.mu.Lock()
	record := &mrRecordConn{Conn: connection, fault: listener.fault, gate: listener.gate}
	listener.connections = append(listener.connections, record)
	listener.mu.Unlock()
	return record, nil
}
func (listener *mrRecordListener) closeConnections() {
	listener.mu.Lock()
	defer listener.mu.Unlock()
	for _, conn := range listener.connections {
		conn.Close()
	}
}
func (listener *mrRecordListener) payloadPositions(key []byte) []int64 {
	listener.mu.Lock()
	defer listener.mu.Unlock()
	var positions []int64
	for _, conn := range listener.connections {
		conn.mu.Lock()
		data := append([]byte(nil), conn.data...)
		conn.mu.Unlock()
		for len(data) >= 8 {
			if string(data[:4]) != "croc" {
				break
			}
			size := int(binary.LittleEndian.Uint32(data[4:8]))
			if size > len(data)-8 {
				break
			}
			payload, err := crypt.Decrypt(data[8:8+size], key)
			if err == nil && len(payload) >= 40 {
				positions = append(positions, int64(binary.LittleEndian.Uint64(payload[32:40])))
			}
			data = data[8+size:]
		}
	}
	return positions
}

type mrTestRelay struct {
	config  multirelay.Relay
	cancel  context.CancelFunc
	data    *mrRecordListener
	control net.Listener
	done    [2]chan error
}

func mrStartRelay(t *testing.T, id string) *mrTestRelay {
	t.Helper()
	control, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	data, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		control.Close()
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	relay := &mrTestRelay{config: multirelay.Relay{ID: id, Address: "127.0.0.1", ControlPort: control.Addr().(*net.TCPAddr).Port, DataPort: data.Addr().(*net.TCPAddr).Port}, cancel: cancel, data: &mrRecordListener{Listener: data}, control: control, done: [2]chan error{make(chan error, 1), make(chan error, 1)}}
	go func() {
		relay.done[0] <- tcp.RunWithListener(control, "multirelay-test", tcp.WithCtx(ctx), tcp.WithLogLevel("error"), tcp.WithBanner(strconv.Itoa(relay.config.DataPort)))
	}()
	go func() {
		relay.done[1] <- tcp.RunWithListener(relay.data, "multirelay-test", tcp.WithCtx(ctx), tcp.WithLogLevel("error"))
	}()
	t.Cleanup(func() {
		relay.stop()
		for _, done := range relay.done {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("relay did not stop")
			}
		}
	})
	return relay
}
func (relay *mrTestRelay) stop() {
	relay.cancel()
	relay.control.Close()
	relay.data.Close()
	relay.data.closeConnections()
}
func mrPool(t *testing.T) (multirelay.Config, []*mrTestRelay) {
	t.Helper()
	var relays []*mrTestRelay
	for _, id := range []string{"a", "b", "c", "d"} {
		relays = append(relays, mrStartRelay(t, id))
	}
	return multirelay.Config{Version: 1, Pairs: []multirelay.Pair{{ID: "primary", Relays: []multirelay.Relay{relays[0].config, relays[1].config}}, {ID: "backup", Relays: []multirelay.Relay{relays[2].config, relays[3].config}}}}, relays
}
func mrConfigPath(t *testing.T, config multirelay.Config) string {
	t.Helper()
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(t.TempDir(), "config.yaml")
	if err = os.WriteFile(name, data, 0600); err != nil {
		t.Fatal(err)
	}
	return name
}
func mrClients(t *testing.T, senderConfig, receiverConfig multirelay.Config) (*Client, *Client, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	makeClient := func(sender bool, config multirelay.Config) *Client {
		client, err := NewCtx(ctx, Options{IsSender: sender, SharedSecret: "5-multirelay-test-secret", Curve: "p256", RelayPassword: "multirelay-test", MultiRelayConfig: mrConfigPath(t, config), OutDir: t.TempDir(), NoPrompt: true, Overwrite: false, NoCompress: true, DisableClipboard: true})
		if err != nil {
			t.Fatal(err)
		}
		client.multi.setupTimeout = 5 * time.Second
		client.multi.idleTimeout = 2 * time.Second
		return client
	}
	return makeClient(true, senderConfig), makeClient(false, receiverConfig), ctx
}
func mrRun(t *testing.T, sender, receiver *Client, size int) ([]byte, error, error) {
	t.Helper()
	contents := make([]byte, size)
	for i := range contents {
		contents[i] = byte((i*31 + i/257) % 251)
	}
	source := filepath.Join(t.TempDir(), "payload.bin")
	if err := os.WriteFile(source, contents, 0600); err != nil {
		t.Fatal(err)
	}
	files, folders, total, err := GetFilesInfo([]string{source}, false, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	sent, received := make(chan error, 1), make(chan error, 1)
	go func() { sent <- sender.Send(files, folders, total) }()
	go func() { received <- receiver.Receive() }()
	var sendErr, receiveErr error
	select {
	case sendErr = <-sent:
	case <-time.After(35 * time.Second):
		t.Fatal("sender hung")
	}
	select {
	case receiveErr = <-received:
	case <-time.After(35 * time.Second):
		t.Fatal("receiver hung")
	}
	if sendErr == nil && receiveErr == nil {
		actual, err := os.ReadFile(filepath.Join(receiver.Options.OutDir, "payload.bin"))
		if err != nil {
			t.Fatal(err)
		}
		if sha256.Sum256(actual) != sha256.Sum256(contents) {
			t.Fatal("source/destination SHA-256 differ")
		}
	}
	return contents, sendErr, receiveErr
}
func TestMultiRelayIntegrationNormalExposure(t *testing.T) {
	config, relays := mrPool(t)
	sender, receiver, _ := mrClients(t, config, config)
	lanes := [2]chan struct{}{make(chan struct{}), make(chan struct{})}
	var started [2]sync.Once
	sender.multi.onSend = func(attempt, lane int, position int64) {
		started[lane].Do(func() {
			close(lanes[lane])
			select {
			case <-lanes[1-lane]:
			case <-time.After(5 * time.Second):
				t.Error("relay workers did not overlap")
			}
		})
	}
	_, sendErr, receiveErr := mrRun(t, sender, receiver, int(7*multiRelayChunk+11))
	if sendErr != nil || receiveErr != nil {
		t.Fatal(sendErr, receiveErr)
	}
	seen := map[int64]bool{}
	for lane, relay := range relays[:2] {
		positions := relay.data.payloadPositions(sender.Key)
		if len(positions) == 0 {
			t.Fatal("relay did not observe payload", lane)
		}
		for _, position := range positions {
			if int(position/multiRelayChunk)%2 != lane {
				t.Fatal("opposite lane exposed", lane, position)
			}
			seen[position] = true
		}
		if len(positions) >= 8 {
			t.Fatal("single relay observed entire payload")
		}
	}
	if len(seen) != 8 {
		t.Fatalf("captured %d unique chunks want 8", len(seen))
	}
	if sender.multi.attempt != 0 || receiver.multi.attempt != 0 {
		t.Fatal("normal transfer used backup")
	}
}
func TestMultiRelayIntegrationFailoverResume(t *testing.T) {
	for _, failedLane := range []int{0, 1} {
		t.Run(fmt.Sprint(failedLane), func(t *testing.T) {
			config, relays := mrPool(t)
			sender, receiver, _ := mrClients(t, config, config)
			var once sync.Once
			var mu sync.Mutex
			validated := map[int64]bool{}
			retained := map[int64]bool{}
			backupSent := map[int64]bool{}
			resent := false
			receiver.multi.onAccept = func(attempt, lane int, position int64) {
				mu.Lock()
				defer mu.Unlock()
				if attempt == 0 {
					validated[position] = true
					if len(validated) == 4 {
						for p := range validated {
							retained[p] = true
						}
						once.Do(func() { relays[failedLane].stop() })
					}
				}
			}
			sender.multi.onSend = func(attempt, lane int, position int64) {
				if attempt > 0 {
					mu.Lock()
					backupSent[position] = true
					if retained[position] {
						resent = true
					}
					mu.Unlock()
				}
			}
			_, sendErr, receiveErr := mrRun(t, sender, receiver, int(96*multiRelayChunk+21))
			if sendErr != nil || receiveErr != nil {
				t.Fatal(sendErr, receiveErr)
			}
			if sender.multi.attempt != 1 || receiver.multi.attempt != 1 {
				t.Fatal("did not replace whole pair")
			}
			for position := range backupSent {
				if validated[position] {
					t.Fatalf("previously verified chunk %d retransmitted", position)
				}
			}
			if len(retained) == 0 || resent {
				t.Fatal("validated progress not preserved", len(retained), resent)
			}
		})
	}
}
func TestMultiRelayIntegrationMismatch(t *testing.T) {
	config, _ := mrPool(t)
	other := config
	other.Pairs = append([]multirelay.Pair(nil), config.Pairs...)
	other.Pairs[1].ID = "different-backup"
	sender, receiver, _ := mrClients(t, config, other)
	_, sendErr, receiveErr := mrRun(t, sender, receiver, int(3*multiRelayChunk))
	if sendErr == nil || receiveErr == nil {
		t.Fatal("mismatched configuration succeeded")
	}
	if !errors.Is(sendErr, errMultiRelayProtocol) && !errors.Is(receiveErr, errMultiRelayProtocol) {
		t.Fatal("no authenticated mismatch", sendErr, receiveErr)
	}
}
func TestMultiRelayIntegrationExhaustion(t *testing.T) {
	config, relays := mrPool(t)
	sender, receiver, _ := mrClients(t, config, config)
	relays[2].stop()
	relays[3].stop()
	var once sync.Once
	receiver.multi.onAccept = func(attempt, lane int, position int64) { once.Do(func() { relays[0].stop() }) }
	_, sendErr, receiveErr := mrRun(t, sender, receiver, int(96*multiRelayChunk))
	if sendErr == nil || receiveErr == nil || !strings.Contains(sendErr.Error(), "no unused multirelay pair") {
		t.Fatal(sendErr, receiveErr)
	}
}

func TestMultiRelayControlReplayAndReflection(t *testing.T) {
	for _, mode := range []string{"replay", "reflection", "corrupt"} {
		t.Run(mode, func(t *testing.T) {
			local, remote := net.Pipe()
			defer local.Close()
			defer remote.Close()
			key := make([]byte, 32)
			client := &Client{Options: Options{IsSender: false}, Key: key, multi: &multiRelayState{}, conn: []*comm.Comm{comm.New(local)}}
			client.secureMultiRelayControl()
			peer := comm.New(remote)
			send := func(role byte, sequence uint64, corrupt bool) ([]byte, error) {
				body := make([]byte, 12)
				body[0] = role
				binary.BigEndian.PutUint64(body[1:9], sequence)
				copy(body[9:], "msg")
				encrypted, err := crypt.Encrypt(body, key)
				if err != nil {
					t.Fatal(err)
				}
				if corrupt {
					encrypted[len(encrypted)-1] ^= 1
				}
				sent := make(chan error, 1)
				go func() { sent <- peer.Send(encrypted) }()
				received, err := client.connection(0).Receive()
				if sendErr := <-sent; sendErr != nil {
					t.Fatal(sendErr)
				}
				return received, err
			}
			if _, err := send(1, 0, false); err != nil {
				t.Fatal(err)
			}
			role, sequence, corrupt := byte(1), uint64(1), false
			switch mode {
			case "replay":
				sequence = 0
			case "reflection":
				role = 0
			case "corrupt":
				corrupt = true
			}
			if _, err := send(role, sequence, corrupt); !errors.Is(err, errMultiRelayProtocol) {
				t.Fatal("interference accepted", err)
			}
		})
	}
}

func TestMultiRelayIntegrationEmptyAndBoundary(t *testing.T) {
	for _, size := range []int{0, int(multiRelayChunk + 1), int(2 * multiRelayChunk), int(9*multiRelayChunk + 1)} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			config, _ := mrPool(t)
			sender, receiver, _ := mrClients(t, config, config)
			_, sendErr, receiveErr := mrRun(t, sender, receiver, size)
			if sendErr != nil || receiveErr != nil {
				t.Fatal(sendErr, receiveErr)
			}
		})
	}
}
func TestMultiRelayIntegrationCompression(t *testing.T) {
	config, _ := mrPool(t)
	sender, receiver, _ := mrClients(t, config, config)
	sender.Options.NoCompress = false
	_, sendErr, receiveErr := mrRun(t, sender, receiver, int(13*multiRelayChunk+53))
	if sendErr != nil || receiveErr != nil {
		t.Fatal(sendErr, receiveErr)
	}
}
func TestMultiRelayIntegrationCancellation(t *testing.T) {
	config, _ := mrPool(t)
	cancelCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	makeClient := func(sending bool) *Client {
		client, err := NewCtx(cancelCtx, Options{IsSender: sending, SharedSecret: "5-cancel-multirelay-secret", Curve: "p256", RelayPassword: "multirelay-test", MultiRelayConfig: mrConfigPath(t, config), OutDir: t.TempDir(), NoPrompt: true, Overwrite: false, NoCompress: true, DisableClipboard: true})
		if err != nil {
			t.Fatal(err)
		}
		return client
	}
	sender, receiver := makeClient(true), makeClient(false)
	receiver.multi.onAccept = func(_, _ int, _ int64) { cancel() }
	_, sendErr, receiveErr := mrRun(t, sender, receiver, int(96*multiRelayChunk))
	if sendErr == nil || receiveErr == nil {
		t.Fatal("cancellation reported success")
	}
}

// Faults act on framed ciphertext at the relay, without client session keys.
func (conn *mrRecordConn) Write(data []byte) (int, error) {
	if conn.fault == "" {
		return conn.Conn.Write(data)
	}
	conn.writeMu.Lock()
	defer conn.writeMu.Unlock()
	original := len(data)
	conn.pending = append(conn.pending, data...)
	for len(conn.pending) >= 8 {
		if string(conn.pending[:4]) != "croc" {
			return 0, errors.New("unexpected unframed relay write")
		}
		length := int(binary.LittleEndian.Uint32(conn.pending[4:8]))
		if length > len(conn.pending)-8 {
			break
		}
		frame := append([]byte(nil), conn.pending[:8+length]...)
		conn.pending = conn.pending[8+length:]
		drop, replay := false, false
		if !conn.injected && length >= int(multiRelayChunk)+40+28 {
			conn.injected = true
			switch conn.fault {
			case "corrupt":
				frame[len(frame)-1] ^= 1
			case "replay":
				replay = true
			case "drop":
				drop = true
			case "delay":
				select {
				case <-conn.gate:
				case <-time.After(5 * time.Second):
					return 0, errors.New("delay gate timed out")
				}
			}
		}
		if drop {
			continue
		}
		if _, err := conn.Conn.Write(frame); err != nil {
			return 0, err
		}
		if replay {
			if _, err := conn.Conn.Write(frame); err != nil {
				return 0, err
			}
		}
	}
	return original, nil
}
func TestMultiRelayIntegrationRelayInterference(t *testing.T) {
	for _, fault := range []string{"corrupt", "replay", "drop", "delay"} {
		t.Run(fault, func(t *testing.T) {
			config, relays := mrPool(t)
			sender, receiver, _ := mrClients(t, config, config)
			gate := make(chan struct{})
			var once sync.Once
			relays[0].data.mu.Lock()
			relays[0].data.fault = fault
			relays[0].data.gate = gate
			relays[0].data.mu.Unlock()
			receiver.multi.onAccept = func(attempt, lane int, position int64) {
				if lane == 1 {
					once.Do(func() { close(gate) })
				}
			}
			_, sendErr, receiveErr := mrRun(t, sender, receiver, int(12*multiRelayChunk+7))
			switch fault {
			case "corrupt":
				if sendErr == nil || receiveErr == nil {
					t.Fatal("corrupt ciphertext accepted")
				}
			default:
				if sendErr != nil || receiveErr != nil {
					t.Fatal(fault, sendErr, receiveErr)
				}
			}
			if fault == "drop" && (sender.multi.attempt != 1 || receiver.multi.attempt != 1) {
				t.Fatal("dropped data did not trigger full-pair recovery")
			}
		})
	}
}

func TestMultiRelayIntegrationIncompatiblePeer(t *testing.T) {
	for _, legacySender := range []bool{true, false} {
		t.Run(fmt.Sprint(legacySender), func(t *testing.T) {
			config, _ := mrPool(t)
			sender, receiver, _ := mrClients(t, config, config)
			legacy := receiver
			if legacySender {
				legacy = sender
			}
			legacy.multi = nil
			legacy.Options.RelayAddress = config.Pairs[0].Relays[0].ControlAddress()
			legacy.Options.RelayAddress6 = ""
			legacy.Options.DisableLocal = true
			// A peer that cannot speak the new protocol must never complete via legacy.
			if sender.multi != nil {
				sender.multi.setupTimeout = 500 * time.Millisecond
			}
			if receiver.multi != nil {
				receiver.multi.setupTimeout = 500 * time.Millisecond
			}
			_, sendErr, receiveErr := mrRun(t, sender, receiver, int(3*multiRelayChunk))
			if sendErr == nil || receiveErr == nil {
				t.Fatal("incompatible peer silently downgraded", sendErr, receiveErr)
			}
		})
	}
}
