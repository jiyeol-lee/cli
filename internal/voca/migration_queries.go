package voca

const (
	vocabularyTableExistsSQL   = `SELECT COUNT(*) FROM sqlite_schema WHERE type = 'table' AND name = 'voca__vocabulary'`
	vocabularyTableInfoSQL     = `SELECT name, type, "notnull", dflt_value, pk FROM pragma_table_info('voca__vocabulary') ORDER BY cid`
	vocabularyUniqueIndexesSQL = `SELECT indexes.name, columns.name
		FROM pragma_index_list('voca__vocabulary') AS indexes
		JOIN pragma_index_info(indexes.name) AS columns
		WHERE indexes."unique" = 1 AND indexes.partial = 0
		ORDER BY indexes.name, columns.seqno`
)
