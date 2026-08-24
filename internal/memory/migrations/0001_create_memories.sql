CREATE TABLE memory__memories (
	id INTEGER PRIMARY KEY,
	scope TEXT NOT NULL CHECK (scope IN ('global', 'project')),
	project_directory TEXT,
	category TEXT NOT NULL CHECK (category IN ('preference', 'convention', 'note')),
	memory TEXT NOT NULL CHECK (length(trim(memory)) > 0),
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	archived_at TEXT,
	CHECK (
		(scope = 'global' AND project_directory IS NULL) OR
		(scope = 'project' AND project_directory IS NOT NULL AND length(trim(project_directory)) > 0)
	)
);

CREATE INDEX memory__memories_scope_archived_idx
	ON memory__memories(scope, archived_at);

CREATE INDEX memory__memories_project_directory_archived_idx
	ON memory__memories(project_directory, archived_at);
