package memory

const (
	insertMemorySQL          = `INSERT INTO memory__memories(scope, project_directory, category, memory) VALUES (?, ?, ?, ?)`
	selectProjectMemoriesSQL = `SELECT id, scope, project_directory, category, memory, updated_at
		FROM memory__memories
		WHERE scope = 'project' AND project_directory = ? AND archived_at IS NULL
		ORDER BY updated_at DESC, id DESC`
	selectGlobalMemoriesSQL = `SELECT id, scope, project_directory, category, memory, updated_at
		FROM memory__memories
		WHERE scope = 'global' AND archived_at IS NULL
		ORDER BY updated_at DESC, id DESC`
	selectAllMemoriesSQL = `SELECT id, scope, project_directory, category, memory, updated_at
		FROM memory__memories
		WHERE archived_at IS NULL AND (scope = 'global' OR (scope = 'project' AND project_directory = ?))
		ORDER BY CASE scope WHEN 'project' THEN 0 ELSE 1 END, updated_at DESC, id DESC`
	archiveProjectMemorySQL = `UPDATE memory__memories
		SET archived_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP
		WHERE id = ? AND scope = 'project' AND project_directory = ? AND archived_at IS NULL`
	archiveGlobalMemorySQL = `UPDATE memory__memories
		SET archived_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP
		WHERE id = ? AND scope = 'global' AND archived_at IS NULL`
)
