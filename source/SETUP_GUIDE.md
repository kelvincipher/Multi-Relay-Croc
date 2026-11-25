# Hướng dẫn chạy

## 1. Build hoặc dùng binary đi kèm

Yêu cầu Go 1.27.3 theo go.mod. Source snapshot nằm trong `source/` của ZIP. Các lệnh dưới đây chạy từ thư mục source đó.

```sh
go version
go build -o croc-multirelay .
```

ZIP có binary Linux amd64 đã chạy demo và Windows amd64 đã cross-compile với `croc_no_tailcat`. Windows binary chưa được chạy trên máy Windows. Không có thay đổi license upstream.

## 2. Demo tự động không cần Docker

Cần Python 3 và binary đã build. Script tạo bốn relay process với port rảnh, cấu hình tạm giống nhau cho hai client, kiểm tra readiness, truyền qua CLI, so SHA-256 và dừng toàn bộ process sau khi chạy.

```sh
python3 scripts/multirelay/demo.py --binary ./croc-multirelay
python3 scripts/multirelay/demo.py --binary ./croc-multirelay --kill a
python3 scripts/multirelay/demo.py --binary ./croc-multirelay --kill b
```

Mỗi lần thành công in JSON có `status: PASS`, scenario, số byte và SHA-256. Kịch bản kill dùng file 8.388.612 byte, chờ dữ liệu xuất hiện ở receiver rồi terminate relay đã chọn. Script kiểm tra cả hai client ghi nhận pair `backup`. Đây là password/code fixture chỉ cho localhost, không phải credential triển khai thực tế.

## 3. Chạy thủ công

Dùng bốn terminal hoặc máy độc lập. Thiết lập cùng `CROC_PASS` ở các relay và client qua môi trường riêng. Nếu từng relay dùng password khác, thêm `password_env` vào entry tương ứng trong YAML và đặt biến đó ở cả hai client.

```sh
./croc-multirelay relay --host 127.0.0.1 --ports 9009,9010
./croc-multirelay relay --host 127.0.0.1 --ports 9019,9020
./croc-multirelay relay --host 127.0.0.1 --ports 9029,9030
./croc-multirelay relay --host 127.0.0.1 --ports 9039,9040
```

Ở sender và receiver chọn cùng bản `examples/multirelay/relays.yaml` bằng đường dẫn tuyệt đối:

```sh
export CROC_MULTIRELAY_CONFIG="$PWD/examples/multirelay/relays.yaml"
./croc-multirelay send secret.pdf
```

File không rỗng phải lớn hơn 32 KiB. Không cần gõ địa chỉ relay khi gửi/nhận. Có thể thay biến môi trường bằng global flag `--multirelay-config /absolute/path/relays.yaml` đặt trước `send` hoặc trước transfer code.

Trên Windows, receiver giữ cách dùng positional code của Croc:

```powershell
$env:CROC_MULTIRELAY_CONFIG = (Resolve-Path examples/multirelay/relays.yaml).Path
.\croc-multirelay-windows-amd64.exe <transfer-code>
```

Commit upstream được chọn không mặc định cho phép code trong command line trên Unix, do code xuất hiện trong process list. Bản mở rộng giữ chính sách đó. Trên Unix, chạy client rồi dán code vào prompt hoặc dùng biến môi trường cho chạy tự động:

```sh
./croc-multirelay
# Hoặc:
CROC_SECRET='<transfer-code>' ./croc-multirelay
```

Bản upstream có `--classic` nếu người dùng chủ động muốn thay chính sách positional code. Demo không thay thiết lập classic của người dùng.

File cấu hình mặc định là `multirelay.yaml` trong thư mục cấu hình Croc: ưu tiên `CROC_CONFIG_DIR`, tiếp theo `$XDG_CONFIG_HOME/croc`, cuối cùng `$HOME/.config/croc` theo quy ước codebase, kể cả Windows. Có thể cài file một lần tại đó để không cần chọn đường dẫn ở mỗi lệnh. File mặc định không tồn tại thì chạy legacy. File được chọn rõ bằng biến hoặc flag mà không tồn tại thì báo lỗi.

Để thử failover thủ công, dùng file đủ lớn và có thể giới hạn upload bằng `--throttleUpload 1m` trước `send`. Sau khi có tiến độ, dừng relay A hoặc B. Cả client phải báo chuyển sang cặp backup C+D. Không đổi YAML khi transfer đang chạy. Sau khi thành công, so sánh `sha256sum` trên Linux hoặc `Get-FileHash -Algorithm SHA256` trên PowerShell.

## 4. Docker Compose tùy chọn

Chưa thực thi trong môi trường phát triển vì không có Docker. Bản demo process ở trên đã được thực thi.

```sh
cp deploy/multirelay/.env.example deploy/multirelay/.env
# Sửa CROC_PASS trong .env riêng, rồi đặt cùng password ở client.
docker compose -f deploy/multirelay/compose.yaml --env-file deploy/multirelay/.env up --build -d
docker compose -f deploy/multirelay/compose.yaml stop relay-a
# Hoặc stop relay-b để thử lỗi lane 1.
docker compose -f deploy/multirelay/compose.yaml down
```

Port chỉ publish trên localhost. Mỗi service chạy user không phải root và filesystem read-only. Cấu hình Compose phục vụ kiểm thử trên một máy, không chứng minh geographic isolation hoặc chịu được host-wide compromise.

## 5. Lỗi thường gặp

- `no unused multirelay pair remains`: hết pool. Kiểm tra cả bốn relay và kết nối ở hai phía rồi bắt đầu logical transfer mới.
- `authenticated ... mismatch`: cả hai đã gặp nhau nhưng config/protocol/attempt không khớp. Sửa cùng cấu hình ở hai đầu, không bỏ kiểm tra fingerprint.
- Không rendezvous được: có thể địa chỉ, password, firewall hoặc config khác ngay cặp đầu. Chưa có authenticated comparison nên không kết luận mismatch đã được xác minh.
- File nhỏ: dùng file demo lớn hơn 32 KiB. Chưa có fragmentation cho một-chunk file.
- Phần file dở dang còn trên receiver: transfer chưa hoàn tất. Trong cùng process, failover tự tiếp tục. Sau khi khởi động client mới, bảng verified cũ không còn.
