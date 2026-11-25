#!/usr/bin/env python3
"""Run four isolated croc relay processes and verify a CLI transfer locally."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import socket
import subprocess
import tempfile
import time


def free_ports(count):
    sockets = []
    try:
        for _ in range(count):
            sock = socket.socket()
            sock.bind(("127.0.0.1", 0))
            sockets.append(sock)
        return [sock.getsockname()[1] for sock in sockets]
    finally:
        for sock in sockets:
            sock.close()


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--binary", required=True)
    parser.add_argument("--kill", choices=["a", "b"])
    args = parser.parse_args()
    binary = str(Path(args.binary).resolve())
    processes = []
    with tempfile.TemporaryDirectory(prefix="croc-multirelay-demo-") as temp:
        root = Path(temp)
        ports = free_ports(8)
        env = os.environ.copy()
        env.update(CROC_NO_UPDATE_CHECK="1", CROC_PASS="croc-multirelay-local-demo",
                   CROC_SECRET="5-local-multirelay-demo", XDG_CONFIG_HOME=str(root / "home"))
        config = {"version": 1, "pairs": []}
        for i, pair_id in enumerate(["primary", "backup"]):
            relays = []
            for lane in range(2):
                index = 2 * i + lane
                relays.append({"id": chr(ord("a") + index), "address": "127.0.0.1",
                               "control_port": ports[index * 2], "data_port": ports[index * 2 + 1]})
            config["pairs"].append({"id": pair_id, "relays": relays})
        selected = root / "relays.yaml"
        selected.write_text(json.dumps(config), encoding="utf-8")  # JSON is YAML 1.2
        env["CROC_MULTIRELAY_CONFIG"] = str(selected)
        handles = []
        try:
            for index in range(4):
                relay_log = open(root / f"relay-{index}.log", "wb")
                handles.append(relay_log)
                processes.append(subprocess.Popen([binary, "relay", "--host", "127.0.0.1", "--ports",
                                                   f"{ports[2*index]},{ports[2*index+1]}"],
                                                  env=env, stdout=relay_log, stderr=relay_log))
            deadline = time.monotonic() + 10
            for port in ports:
                while True:
                    try:
                        with socket.create_connection(("127.0.0.1", port), timeout=0.1):
                            break
                    except OSError:
                        if time.monotonic() >= deadline:
                            raise RuntimeError("relay readiness timeout")
                        time.sleep(0.03)
            source = root / "payload.bin"
            block = bytes((i * 31) % 251 for i in range(32768))
            source.write_bytes(block * (256 if args.kill else 8) + b"tail")
            output = root / "received"
            output.mkdir()
            sender_log = open(root / "sender.log", "wb")
            receiver_log = open(root / "receiver.log", "wb")
            handles.extend([sender_log, receiver_log])
            sender = subprocess.Popen([binary, "--no-update-check", "--disable-clipboard", "--ignore-stdin",
                                       "--throttleUpload", "4m", "send", str(source)],
                                      env=env, stdin=subprocess.DEVNULL, stdout=sender_log, stderr=sender_log)
            receiver = subprocess.Popen([binary, "--no-update-check", "--yes", "--out", str(output)],
                                        env=env, stdin=subprocess.DEVNULL, stdout=receiver_log, stderr=receiver_log)
            processes.extend([sender, receiver])
            if args.kill:
                destination = output / source.name
                deadline = time.monotonic() + 15
                while True:
                    if destination.exists():
                        with destination.open("rb") as stream:
                            if stream.read(16) == block[:16]:
                                break
                    if sender.poll() is not None or time.monotonic() >= deadline:
                        raise RuntimeError("no verified transfer progress before failure injection")
                    time.sleep(0.005)
                processes[0 if args.kill == "a" else 1].terminate()
            send_status = sender.wait(timeout=30)
            receive_status = receiver.wait(timeout=30)
            for handle in handles:
                handle.flush()
            if send_status or receive_status:
                # Demo fixtures contain no user data. Keep failure output useful.
                raise RuntimeError(f"CLI failed: sender={send_status} receiver={receive_status}\n"
                                   + (root / "sender.log").read_text(errors="replace")
                                   + (root / "receiver.log").read_text(errors="replace"))
            expected = hashlib.sha256(source.read_bytes()).hexdigest()
            actual = hashlib.sha256((output / source.name).read_bytes()).hexdigest()
            if expected != actual:
                raise RuntimeError(f"SHA-256 mismatch: expected={expected} actual={actual} bytes={(output/source.name).stat().st_size}\n" + (root/"sender.log").read_text(errors="replace") + (root/"receiver.log").read_text(errors="replace"))
            if args.kill:
                for log in ["sender.log", "receiver.log"]:
                    if "reconnecting with unused pair backup" not in (root / log).read_text(errors="replace"):
                        raise RuntimeError("full-pair failover not observed by both clients")
            print(json.dumps({"status": "PASS", "scenario": "kill-" + args.kill if args.kill else "normal",
                              "bytes": source.stat().st_size, "sha256": actual}))
        finally:
            for process in reversed(processes):
                if process.poll() is None:
                    process.terminate()
            for process in processes:
                try:
                    process.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait()
            for handle in handles:
                handle.close()


if __name__ == "__main__":
    main()
