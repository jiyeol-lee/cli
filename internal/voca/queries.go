package voca

const (
	insertVocabularySQL = `INSERT INTO voca__vocabulary(word, created_at, updated_at) VALUES (?, ?, ?)`
	deleteVocabularySQL = `DELETE FROM voca__vocabulary WHERE word = ?`
	listVocabularySQL   = `SELECT id, word, read_count FROM voca__vocabulary ORDER BY word, id`
	leastReadRandomSQL  = `SELECT id, word, read_count FROM voca__vocabulary WHERE read_count = (SELECT MIN(read_count) FROM voca__vocabulary) ORDER BY random() LIMIT 1`
	randomLimitSQL      = `SELECT id, word, read_count FROM voca__vocabulary ORDER BY random() LIMIT ?`
	incrementSQL        = `UPDATE voca__vocabulary SET read_count = read_count + 1, updated_at = ? WHERE id = ?`
)
