Golden responses and the PUT log captured from the Rust server.

The format, the normalisation rules and the way to redo the capture are documented in
internal/store/testdata/README.txt (section "Golden files" and "How to redo the capture").

rust/*.json        one golden per request (275 files)
rust-order.txt     golden names in the order the requests were sent (the order decides autoincrement ids)
rust-hue-puts.jsonl  PUT requests the Rust server sent to the fake Hue bridge
