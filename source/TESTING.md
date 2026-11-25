# Kiểm thử

Các lệnh dưới đây chạy từ root source với Go 1.27.3. Kết quả thực tế và log nằm trong `docs/multirelay/test-results/`. Log chỉ được bỏ ANSI và chuẩn hóa khoảng trắng để đọc và tạo patch.

## Unit và race tests

```sh
go test -race ./src/multirelay ./src/comm ./src/crypt ./src/pakekey
go test -race ./src/croc -run '^TestMultiRelay' -count=1
go test ./src/utils -run '^TestMultiRelaySHA256$' -count=1
go test -race ./src/cli -run '^TestMultiRelayCLISelection$' -count=1
```

Config tests bao phủ YAML strict, duplicate ID/port/alias, config vượt giới hạn, canonical fingerprint phụ thuộc thứ tự pool, thứ tự selection, pair exhaustion, password environment, explicit missing config, thư mục mặc định và attempt agreement. Unit test reconstruction nhận chunk theo thứ tự 3,1,0,2, xử lý zero bytes, replay, parity sai và phát hiện thay đổi trên đĩa khi resume. Chunk chính cố định 32 KiB theo Croc, các test boundary thay đổi chiều dài chunk cuối, không có flag chỉnh chunk size.

## Đối chiếu integration tests

| Yêu cầu | Test đã thực thi | Kết quả |
|---|---|---|
| T1, T8: A+B và SHA-256 | `TestMultiRelayIntegrationNormalExposure` | PASS |
| T2: exposure từng relay | Cùng test, ghi stream network phía relay và phân loại frame sau transfer | PASS, mỗi relay chỉ thấy parity của nó |
| Worker concurrent | Barrier ở lần gửi đầu của mỗi worker, cả hai phải đến barrier | PASS |
| T3: ngắt A | `TestMultiRelayIntegrationFailoverResume/0` | PASS |
| T4: ngắt B | `TestMultiRelayIntegrationFailoverResume/1` | PASS |
| T5: giữ tiến độ | Cùng test, chụp tập chunk đã xác thực và kiểm tra backup không gửi lại chúng | PASS |
| T6: config mismatch | `TestMultiRelayIntegrationMismatch` | PASS, mismatch qua authenticated channel |
| T7: hết backup | `TestMultiRelayIntegrationExhaustion` | PASS, trả lỗi, không fallback |
| T9: ciphertext bị sửa | `TestMultiRelayIntegrationRelayInterference/corrupt` | PASS, không nhận thành công |
| T9: replay data | `.../replay` | PASS, duplicate không tăng tiến độ |
| T9: drop data | `.../drop` | PASS, full-pair retry và chỉ lấy phần thiếu |
| T9: delay và arrival khác thứ tự | `.../delay` | PASS, toàn file khớp |
| T9: replay/reflection/corrupt control | `TestMultiRelayControlReplayAndReflection` | PASS |
| Peer không bật protocol | `TestMultiRelayIntegrationIncompatiblePeer` | PASS ở cả vai trò sender/receiver legacy |
| File rỗng và boundary | `TestMultiRelayIntegrationEmptyAndBoundary` | PASS |
| File một chunk | `TestMultiRelaySmallFilesFailClosed` | PASS, từ chối an toàn |
| Compression | `TestMultiRelayIntegrationCompression` | PASS |
| Nhiều file | `TestMultiRelayIntegrationMultipleFiles` | PASS với race detector |
| Cancellation | `TestMultiRelayIntegrationCancellation` | PASS, không báo thành công |

Integration Go khởi chạy relay TCP implementation thật trên loopback listeners, với hai listener cho mỗi relay instance. Fault injection đặt ở network wrapper của server, không thay bằng một mock hàm truyền file. Test kill dừng context/listener/connections của relay. Test mismatch chỉ thay ID ở backup nên primary rendezvous vẫn thành công và fingerprint khác biệt có thể được xác thực.

Exposure test thu ciphertext và dùng traffic key của endpoint do test harness giữ để xác định offset. Đây là phương tiện kiểm thử, không phải khả năng của relay trong threat model. Capture không chứa key trong log.

## Kiểm thử CLI với process độc lập

```sh
go build -o croc-multirelay .
python3 scripts/multirelay/demo.py --binary ./croc-multirelay
python3 scripts/multirelay/demo.py --binary ./croc-multirelay --kill a
python3 scripts/multirelay/demo.py --binary ./croc-multirelay --kill b
```

Đã chạy cả ba. Normal: 262.148 byte, SHA-256 `3363764fb5d0081f5369a4c9b0a481e07e96a70eef23e1691d78819c0b56e5b6`. Kill A và kill B: 8.388.612 byte, SHA-256 `d39c5647373e8e94a7fd07000ba980949406ee4a928f4b856aa170238244d1bd`. Hai client đều chuyển sang backup và kết quả khớp source. Script tự dọn relay và client process.

## Regression

```sh
go test ./src/croc ./src/comm ./src/crypt ./src/tcp ./src/pakekey ./src/multirelay
go test ./src/cli -skip '^TestStoreCommandUploads$' -count=1
go test ./src/utils -skip '^TestLocalIP$' -count=1
```

Các lệnh này đã PASS. Chạy toàn bộ `src/cli` và `src/utils` trước đó phát hiện hai nhóm lỗi cũng tái hiện trên worktree nguyên bản ở đúng base commit:

- `TestLocalIP`: sandbox không có route UDP `8.8.8.8:80`, netlink không được phép, test không tìm được địa chỉ local như kỳ vọng.
- `TestStoreCommandUploads`: trả `upstream failure` ở bốn subcase, cũng thất bại trên baseline. Không đổi test để che lỗi. Đây là lỗi môi trường quan sát được, nguyên nhân sâu hơn của upstream failure chưa được xác định.

Vì vậy không tuyên bố toàn bộ `go test ./...` pass. Toàn bộ suite repository chưa được chạy. Phạm vi regression đã chọn bao gồm core transfer, relay, crypto, peer authentication, CLI và utilities với hai nhóm loại trừ có đối chứng baseline.

## Chưa xác minh

- Chạy binary trên Windows thật.
- Build/chạy Docker image và Compose với Docker engine.
- Geographic isolation, cloud deployment và provider-wide threat model.
- Stress/soak dài hạn, mọi lịch reconnect bất đối xứng, file cỡ TiB hoặc audit độc lập.
- Resume sau khi process bị khởi động lại. Bản hiện tại chỉ giữ verified state trong RAM.
