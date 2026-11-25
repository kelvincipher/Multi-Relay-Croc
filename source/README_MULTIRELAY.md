# Croc Multi-Relay Security Extension

Bản mở rộng dựa trên Croc `main`, commit `d77d59a8931d67460574f7603b9446470d475070`, module `github.com/schollz/croc/v11`, Go `1.27.3`.

Client truyền các chunk 32 KiB qua hai relay instance độc lập. Chunk có chỉ số chẵn đi qua lane 0, chỉ số lẻ qua lane 1. Hai worker chạy đồng thời. Một relay chỉ thấy ciphertext của lane của nó. PAKE v2 và AES-GCM của Croc được tái sử dụng. Hash SHA-256 toàn file được kiểm tra trước khi receiver báo hoàn tất.

Khi một lane hoặc control connection lỗi, client đóng cả cặp, đợi worker cũ kết thúc, chuyển sang cặp kế tiếp trong pool và thực hiện PAKE mới. Receiver giữ digest của những chunk đã xác thực và ghi đầy đủ, kiểm tra lại chúng trên đĩa rồi chỉ yêu cầu các range thiếu. Không dùng nhận diện vùng zero của cơ chế resume legacy cho chế độ này.

Chế độ tự bật khi có `multirelay.yaml` trong thư mục cấu hình Croc. Có thể chọn file bằng biến `CROC_MULTIRELAY_CONFIG` hoặc global flag `--multirelay-config`. Hai đầu phải có cùng cấu hình hiệu lực và hỗ trợ protocol. Không tìm thấy file mặc định thì Croc hoạt động như trước. File được chỉ định rõ mà thiếu hoặc sai sẽ gây lỗi. Không có đường fallback tự động sang một relay, local relay, Tailcat, P2P hoặc stored transfer.

Đọc [SETUP_GUIDE.md](SETUP_GUIDE.md) để chạy. Demo tự tạo bốn relay process, chạy sender/receiver bằng CLI và so sánh SHA-256:

```sh
go build -o croc-multirelay .
python3 scripts/multirelay/demo.py --binary ./croc-multirelay
python3 scripts/multirelay/demo.py --binary ./croc-multirelay --kill a
python3 scripts/multirelay/demo.py --binary ./croc-multirelay --kill b
```

## Giới hạn cụ thể

- File không rỗng phải lớn hơn 32.768 byte. File từ 1 đến 32.768 byte bị từ chối trước payload vì chỉ tạo một chunk. Đây là giới hạn có chủ đích để giữ S1. Chưa triển khai ciphertext fragmentation cho file nhỏ. File rỗng được xử lý bằng metadata.
- Không hỗ trợ symlink trong Multi-Relay. Croc legacy giữ nguyên khả năng của nó. Nhiều file thông thường và compression đã có integration test.
- Resume tự động giữ trạng thái trong cùng process. Khởi động lại client không phục hồi bảng digest từ checkpoint trên đĩa.
- Setup/rendezvous tối đa 120 giây cho mỗi giai đoạn có watchdog, idle khi đang truyền là 45 giây. Pool hữu hạn, mỗi cặp chỉ dùng một lần. Chậm quá ngưỡng có thể làm hết pool và kết thúc an toàn. Không bảo đảm liveness trước DoS hoặc mọi lịch mạng bất đối xứng.
- File đích có thể còn ở trạng thái dở dang nếu chuyển thất bại. Chỉ coi thành công khi client báo hoàn tất và SHA-256 khớp.
- Digest lưu trong RAM tăng theo số chunk. Không có benchmark cho file cỡ TiB.
- Kiểm tra ID, endpoint, localhost/IP aliases và DNS alias giúp phát hiện cấu hình trùng. Phần mềm không chứng thực được rằng hai endpoint thực sự thuộc hai máy hay hai process cách ly. Người triển khai chịu trách nhiệm về tính độc lập đó.
- Tăng mức phân tách quan sát ciphertext, không tăng độ mạnh mật mã của AES-GCM. Không cung cấp anonymity hay perfect secrecy. Không bảo vệ trước chiếm quyền hai relay, endpoint bị xâm nhập, provider-wide compromise, traffic analysis hoặc DoS.
- Bốn process trên localhost chỉ chứng minh chức năng. Triển khai thực tế cần các instance cách ly và đặt ở vị trí địa lý phù hợp. Cùng provider được phép theo yêu cầu.
- Windows binary được cross-compile, chưa chạy trên Windows. Docker Compose được cung cấp nhưng chưa thực thi vì môi trường không có Docker.

Xem [ARCHITECTURE.md](ARCHITECTURE.md), [TESTING.md](TESTING.md) và [IMPLEMENTATION_REPORT.md](IMPLEMENTATION_REPORT.md) để đối chiếu protocol, kết quả kiểm thử và các mục chưa xác minh.
