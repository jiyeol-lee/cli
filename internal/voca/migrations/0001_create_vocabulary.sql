CREATE TABLE voca__vocabulary (
    id INTEGER PRIMARY KEY,
    word TEXT NOT NULL UNIQUE,
    read_count INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
