-- v3 → v4：上传校验码可在收片期间并行计算；任务完成前记录待验证的客户端摘要。
ALTER TABLE upload_tasks
    ADD COLUMN expected_checksum TEXT NOT NULL DEFAULT '';
