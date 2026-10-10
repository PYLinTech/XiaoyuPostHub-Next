DELETE FROM system_config
WHERE key IN ('upload.max_file_size', 'upload.max_tasks', 'upload.max_concurrency');
