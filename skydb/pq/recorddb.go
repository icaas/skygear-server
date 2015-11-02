package pq

import (
	"bytes"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/jmoiron/sqlx"
	sq "github.com/lann/squirrel"
	"github.com/lib/pq"
	"github.com/oursky/skygear/skydb"
	"github.com/paulmach/go.geo"
)

// This file implements Record related operations of the
// skydb/pq implementation.

// Different data types that can be saved in and loaded from postgreSQL
// NOTE(limouren): varchar is missing because text can replace them,
// see the docs here: http://www.postgresql.org/docs/9.4/static/datatype-character.html
const (
	TypeString    = "text"
	TypeNumber    = "double precision"
	TypeBoolean   = "boolean"
	TypeJSON      = "jsonb"
	TypeTimestamp = "timestamp without time zone"
	TypeLocation  = "geometry(Point)"
)

type nullJSON struct {
	JSON  interface{}
	Valid bool
}

func (nj *nullJSON) Scan(value interface{}) error {
	data, ok := value.([]byte)
	if value == nil || !ok {
		nj.JSON = nil
		nj.Valid = false
		return nil
	}

	err := json.Unmarshal(data, &nj.JSON)
	nj.Valid = err == nil

	return err
}

type assetValue skydb.Asset

func (asset assetValue) Value() (driver.Value, error) {
	return asset.Name, nil
}

type nullAsset struct {
	Asset skydb.Asset
	Valid bool
}

func (na *nullAsset) Scan(value interface{}) error {
	if value == nil {
		na.Asset = skydb.Asset{}
		na.Valid = false
		return nil
	}

	assetName, ok := value.([]byte)
	if !ok {
		return fmt.Errorf("failed to scan Asset: got type(value) = %T, expect []byte", value)
	}

	na.Asset = skydb.Asset{
		Name: string(assetName),
	}
	na.Valid = true

	return nil
}

type nullLocation struct {
	Location skydb.Location
	Valid    bool
}

func (nl *nullLocation) Scan(value interface{}) error {
	if value == nil {
		nl.Location = skydb.Location{}
		nl.Valid = false
		return nil
	}

	src, ok := value.([]byte)
	if !ok {
		return fmt.Errorf("failed to scan Location: got type(value) = %T, expect []byte", value)
	}

	// TODO(limouren): instead of decoding a str-encoded hex, we should utilize
	// ST_AsBinary to perform the SELECT
	decoded := make([]byte, hex.DecodedLen(len(src)))
	_, err := hex.Decode(decoded, src)
	if err != nil {
		return fmt.Errorf("failed to scan Location: malformed wkb")
	}

	err = (*geo.Point)(&nl.Location).Scan(decoded)
	nl.Valid = err == nil
	return err
}

type referenceValue skydb.Reference

func (ref referenceValue) Value() (driver.Value, error) {
	return ref.ID.Key, nil
}

type jsonSliceValue []interface{}

func (s jsonSliceValue) Value() (driver.Value, error) {
	return json.Marshal([]interface{}(s))
}

type jsonMapValue map[string]interface{}

func (m jsonMapValue) Value() (driver.Value, error) {
	return json.Marshal(map[string]interface{}(m))
}

type aclValue skydb.RecordACL

func (acl aclValue) Value() (driver.Value, error) {
	if acl == nil {
		return nil, nil
	}
	return json.Marshal(acl)
}

type locationValue skydb.Location

func (loc *locationValue) Value() (driver.Value, error) {
	return (*geo.Point)(loc).ToWKT(), nil
}

func (db *database) Get(id skydb.RecordID, record *skydb.Record) error {
	typemap, err := db.remoteColumnTypes(id.Type)
	if err != nil {
		return err
	}

	if len(typemap) == 0 { // record type has not been created
		return skydb.ErrRecordNotFound
	}

	sqlStmt, args, err := db.selectQuery(id.Type, typemap).Where("_id = ?", id.Key).ToSql()
	if err != nil {
		panic(err)
	}

	log.WithFields(log.Fields{
		"sql":  sqlStmt,
		"args": args,
	}).Debugln("Getting record")

	row := db.Db.QueryRowx(sqlStmt, args...)
	if err := newRecordScanner(id.Type, typemap, row).Scan(record); err == sql.ErrNoRows {
		return skydb.ErrRecordNotFound
	} else if err != nil {
		return err
	}
	return nil
}

// Save attempts to do a upsert
func (db *database) Save(record *skydb.Record) error {
	if record.ID.Key == "" {
		return errors.New("db.save: got empty record id")
	}
	if record.ID.Type == "" {
		return fmt.Errorf("db.save %s: got empty record type", record.ID.Key)
	}
	if record.OwnerID == "" {
		return fmt.Errorf("db.save %s: got empty OwnerID", record.ID.Key)
	}

	pkData := map[string]interface{}{
		"_id":          record.ID.Key,
		"_database_id": db.userID,
	}
	upsert := upsertQuery(db.tableName(record.ID.Type), pkData, convert(record)).
		IgnoreKeyOnUpdate("_owner_id").
		IgnoreKeyOnUpdate("_created_at").
		IgnoreKeyOnUpdate("_created_by")

	_, err := execWith(db.Db, upsert)
	if err != nil {
		sql, args, _ := upsert.ToSql()
		log.WithFields(log.Fields{
			"sql":  sql,
			"args": args,
			"err":  err,
		}).Debugln("Failed to save record")

		return err
	}

	record.DatabaseID = db.userID
	return nil
}

func convert(r *skydb.Record) map[string]interface{} {
	m := map[string]interface{}{}
	for key, rawValue := range r.Data {
		switch value := rawValue.(type) {
		case []interface{}:
			m[key] = jsonSliceValue(value)
		case map[string]interface{}:
			m[key] = jsonMapValue(value)
		case skydb.Asset:
			m[key] = assetValue(value)
		case skydb.Reference:
			m[key] = referenceValue(value)
		case *skydb.Location:
			m[key] = (*locationValue)(value)
		default:
			m[key] = rawValue
		}
	}
	m["_owner_id"] = r.OwnerID
	m["_access"] = aclValue(r.ACL)
	m["_created_at"] = r.CreatedAt
	m["_created_by"] = r.CreatorID
	m["_updated_at"] = r.UpdatedAt
	m["_updated_by"] = r.UpdaterID
	return m
}

func (db *database) Delete(id skydb.RecordID) error {
	builder := psql.Delete(db.tableName(id.Type)).
		Where("_id = ? AND _database_id = ?", id.Key, db.userID)

	result, err := execWith(db.Db, builder)
	if isUndefinedTable(err) {
		return skydb.ErrRecordNotFound
	} else if err != nil {
		sql, args, _ := builder.ToSql()
		log.WithFields(log.Fields{
			"id":   id,
			"sql":  sql,
			"args": args,
			"err":  err,
		}).Errorln("Failed to execute delete record statement")
		return fmt.Errorf("delete %s: failed to delete record", id)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		sql, args, _ := builder.ToSql()
		log.WithFields(log.Fields{
			"id":   id,
			"sql":  sql,
			"args": args,
			"err":  err,
		}).Errorln("Failed to fetch rowsAffected")
		return fmt.Errorf("delete %s: failed to retrieve deletion status", id)
	}

	if rowsAffected == 0 {
		return skydb.ErrRecordNotFound
	} else if rowsAffected > 1 {
		sql, args, _ := builder.ToSql()
		log.WithFields(log.Fields{
			"id":           id,
			"sql":          sql,
			"args":         args,
			"rowsAffected": rowsAffected,
			"err":          err,
		}).Errorln("Unexpected rows deleted")
		return fmt.Errorf("delete %s: got %v rows deleted, want 1", id, rowsAffected)
	}

	return err
}

type compoundPredicateSqlizer struct {
	skydb.Predicate
}

type comparisonPredicateSqlizer struct {
	skydb.Predicate
}

func newPredicateSqlizer(predicate skydb.Predicate) sq.Sqlizer {
	if predicate.Operator.IsCompound() {
		return &compoundPredicateSqlizer{predicate}
	}

	return &comparisonPredicateSqlizer{predicate}
}

func (p *compoundPredicateSqlizer) ToSql() (sql string, args []interface{}, err error) {
	switch p.Operator {
	default:
		err = fmt.Errorf("Compound operator `%v` is not supported.", p.Operator)
		return
	case skydb.And:
		and := make(sq.And, len(p.Children))
		for i, child := range p.Children {
			and[i] = newPredicateSqlizer(child.(skydb.Predicate))
		}
		return and.ToSql()
	case skydb.Or:
		or := make(sq.Or, len(p.Children))
		for i, child := range p.Children {
			or[i] = newPredicateSqlizer(child.(skydb.Predicate))
		}
		return or.ToSql()
	case skydb.Not:
		pred := p.Children[0].(skydb.Predicate)
		sql, args, err = newPredicateSqlizer(pred).ToSql()
		sql = fmt.Sprintf("NOT (%s)", sql)
		return
	}
}

func toSqlOperand(expr skydb.Expression) (sql string, args []interface{}) {
	switch expr.Type {
	case skydb.KeyPath:
		sql = pq.QuoteIdentifier(expr.Value.(string))
	case skydb.Function:
		sql, args = funcToSqlOperand(expr.Value.(skydb.Func))
	default:
		sql, args = literalToSqlOperand(expr.Value)
	}
	return
}

func funcToSqlOperand(fun skydb.Func) (string, []interface{}) {
	switch f := fun.(type) {
	case *skydb.DistanceFunc:
		sql := fmt.Sprintf("ST_Distance_Sphere(%s, ST_MakePoint(?, ?))", pq.QuoteIdentifier(f.Field))
		args := []interface{}{f.Location.Lng(), f.Location.Lat()}
		return sql, args
	default:
		panic(fmt.Errorf("got unrecgonized skydb.Func = %T", fun))
	}
}

func literalToSqlOperand(literal interface{}) (string, []interface{}) {
	// Array detection is borrowed from squirrel's expr.go
	switch literalValue := literal.(type) {
	case []interface{}:
		args := make([]interface{}, len(literalValue))
		for i, val := range literalValue {
			args[i] = literalToSqlValue(val)
		}
		return "(" + sq.Placeholders(len(literalValue)) + ")", args
	default:
		return sq.Placeholders(1), []interface{}{literalToSqlValue(literal)}
	}
}

func literalToSqlValue(value interface{}) interface{} {
	switch v := value.(type) {
	case skydb.Reference:
		return v.ID.Key
	default:
		return value
	}
}

func (p *comparisonPredicateSqlizer) ToSql() (sql string, args []interface{}, err error) {
	args = []interface{}{}
	if p.Operator.IsBinary() {
		var buffer bytes.Buffer
		lhs := p.Children[0].(skydb.Expression)
		rhs := p.Children[1].(skydb.Expression)

		sqlOperand, opArgs := toSqlOperand(lhs)
		buffer.WriteString(sqlOperand)
		args = append(args, opArgs...)

		switch p.Operator {
		default:
			err = fmt.Errorf("Comparison operator `%v` is not supported.", p.Operator)
			return
		case skydb.Equal:
			buffer.WriteString(`=`)
		case skydb.GreaterThan:
			buffer.WriteString(`>`)
		case skydb.LessThan:
			buffer.WriteString(`<`)
		case skydb.GreaterThanOrEqual:
			buffer.WriteString(`>=`)
		case skydb.LessThanOrEqual:
			buffer.WriteString(`<=`)
		case skydb.NotEqual:
			buffer.WriteString(`<>`)
		case skydb.Like:
			buffer.WriteString(` LIKE `)
		case skydb.ILike:
			buffer.WriteString(` ILIKE `)
		case skydb.In:
			buffer.WriteString(` IN `)
		}

		sqlOperand, opArgs = toSqlOperand(rhs)
		buffer.WriteString(sqlOperand)
		args = append(args, opArgs...)

		sql = buffer.String()
	} else {
		err = fmt.Errorf("Comparison operator `%v` is not supported.", p.Operator)
	}

	return
}

func (db *database) Query(query *skydb.Query) (*skydb.Rows, error) {
	if query.Type == "" {
		return nil, errors.New("got empty query type")
	}

	typemap, err := db.remoteColumnTypes(query.Type)
	if err != nil {
		return nil, err
	}

	if len(typemap) == 0 { // record type has not been created
		return skydb.EmptyRows, nil
	}

	if query.DesiredKeys != nil {
		newtypemap, err := whitelistedRecordSchema(typemap, query.DesiredKeys)
		if err != nil {
			return nil, err
		}
		typemap = newtypemap
	}

	for key, value := range query.ComputedKeys {
		typemap["_transient_"+key] = skydb.FieldType{
			Type:       skydb.TypeNumber,
			Expression: &value,
		}
	}

	q := db.selectQuery(query.Type, typemap)

	if p := query.Predicate; p != nil {
		q = q.Where(newPredicateSqlizer(*p))
	}

	for _, sort := range query.Sorts {
		orderBy, err := sortOrderBySQL(sort)
		if err != nil {
			return nil, err
		}
		q = q.OrderBy(orderBy)
	}

	if query.ReadableBy != "" {
		// FIXME: Serialize the json instead of building manually
		q = q.Where(
			`(_access @> '[{"user_id":"`+query.ReadableBy+`"}]' OR `+
				`_access IS NULL OR `+
				`_owner_id = ?)`, query.ReadableBy)
	}

	if query.Limit > 0 {
		q = q.Limit(query.Limit)
	}

	if query.Offset > 0 {
		q = q.Offset(query.Offset)
	}

	rows, err := queryWith(db.Db, q)
	return newRows(query.Type, typemap, rows, err)
}

func whitelistedRecordSchema(schema skydb.RecordSchema, whitelistKeys []string) (skydb.RecordSchema, error) {
	wlSchema := skydb.RecordSchema{}

	for _, key := range whitelistKeys {
		columnType, ok := schema[key]
		if !ok {
			return nil, fmt.Errorf(`unexpected key "%s"`, key)
		}
		wlSchema[key] = columnType
	}
	for key, value := range schema {
		if strings.HasPrefix(key, "_") {
			wlSchema[key] = value
		}
	}

	return wlSchema, nil
}

func sortOrderBySQL(sort skydb.Sort) (string, error) {
	var expr string

	switch {
	case sort.KeyPath != "":
		expr = pq.QuoteIdentifier(sort.KeyPath)
	case sort.Func != nil:
		var err error
		expr, err = funcOrderBySQL(sort.Func)
		if err != nil {
			return "", err
		}
	default:
		return "", errors.New("invalid Sort: specify either KeyPath or Func")
	}

	order, err := sortOrderOrderBySQL(sort.Order)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf(expr + " " + order), nil
}

// due to sq not being able to pass args in OrderBy, we can't re-use funcToSqlOperand
func funcOrderBySQL(fun skydb.Func) (string, error) {
	switch f := fun.(type) {
	case *skydb.DistanceFunc:
		sql := fmt.Sprintf(
			"ST_Distance_Sphere(%s, ST_MakePoint(%f, %f))",
			pq.QuoteIdentifier(f.Field),
			f.Location.Lng(),
			f.Location.Lat(),
		)
		return sql, nil
	default:
		return "", fmt.Errorf("got unrecgonized skydb.Func = %T", fun)
	}
}

func sortOrderOrderBySQL(order skydb.SortOrder) (string, error) {
	switch order {
	case skydb.Asc:
		return "ASC", nil
	case skydb.Desc:
		return "DESC", nil
	default:
		return "", fmt.Errorf("unknown sort order = %v", order)
	}
}

// columnsScanner wraps over sqlx.Rows and sqlx.Row to provide
// a consistent interface for column scanning.
type columnsScanner interface {
	Columns() ([]string, error)
	Scan(dest ...interface{}) error
}

type recordScanner struct {
	recordType string
	typemap    skydb.RecordSchema
	cs         columnsScanner
	columns    []string
	err        error
}

func newRecordScanner(recordType string, typemap skydb.RecordSchema, cs columnsScanner) *recordScanner {
	columns, err := cs.Columns()
	return &recordScanner{recordType, typemap, cs, columns, err}
}

func (rs *recordScanner) Scan(record *skydb.Record) error {
	if rs.err != nil {
		return rs.err
	}

	values := make([]interface{}, 0, len(rs.columns))
	for _, column := range rs.columns {
		schema, ok := rs.typemap[column]
		if !ok {
			return fmt.Errorf("received unknown column = %s", column)
		}
		switch schema.Type {
		case skydb.TypeNumber:
			var number sql.NullFloat64
			values = append(values, &number)
		case skydb.TypeString, skydb.TypeReference, skydb.TypeACL:
			var str sql.NullString
			values = append(values, &str)
		case skydb.TypeDateTime:
			var ts pq.NullTime
			values = append(values, &ts)
		case skydb.TypeBoolean:
			var boolean sql.NullBool
			values = append(values, &boolean)
		case skydb.TypeAsset:
			var asset nullAsset
			values = append(values, &asset)
		case skydb.TypeJSON:
			var j nullJSON
			values = append(values, &j)
		case skydb.TypeLocation:
			var l nullLocation
			values = append(values, &l)
		default:
			return fmt.Errorf("received unknown data type = %v for column = %s", schema.Type, column)
		}
	}

	if err := rs.cs.Scan(values...); err != nil {
		rs.err = err
		return err
	}

	record.ID.Type = rs.recordType
	record.Data = map[string]interface{}{}

	for i, column := range rs.columns {
		value := values[i]
		switch svalue := value.(type) {
		default:
			return fmt.Errorf("received unexpected scanned type = %T for column = %s", value, column)
		case *sql.NullFloat64:
			if svalue.Valid {
				record.Set(column, svalue.Float64)
			}
		case *sql.NullString:
			if svalue.Valid {
				schema := rs.typemap[column]
				if schema.Type == skydb.TypeReference {
					record.Set(column, skydb.NewReference(schema.ReferenceType, svalue.String))
				} else if schema.Type == skydb.TypeACL {
					acl := skydb.RecordACL{}
					json.Unmarshal([]byte(svalue.String), &acl)
					record.Set(column, acl)
				} else {
					record.Set(column, svalue.String)
				}
			}
		case *pq.NullTime:
			if svalue.Valid {
				// it is to support direct deep-equal of value between
				// a empty record and a record materialized from the database
				if svalue.Time.IsZero() {
					record.Set(column, time.Time{})
				} else {
					record.Set(column, svalue.Time.In(time.UTC))
				}
			}
		case *sql.NullBool:
			if svalue.Valid {
				record.Set(column, svalue.Bool)
			}
		case *nullAsset:
			if svalue.Valid {
				record.Set(column, svalue.Asset)
			}
		case *nullJSON:
			if svalue.Valid {
				record.Set(column, svalue.JSON)
			}
		case *nullLocation:
			if svalue.Valid {
				record.Set(column, &svalue.Location)
			}
		}
	}

	return nil
}

type rowsIter struct {
	rows *sqlx.Rows
	rs   *recordScanner
}

func (rowsi rowsIter) Close() error {
	return rowsi.rows.Close()
}

func (rowsi rowsIter) Next(record *skydb.Record) error {
	if rowsi.rows.Next() {
		return rowsi.rs.Scan(record)
	} else if rowsi.rows.Err() != nil {
		return rowsi.rows.Err()
	} else {
		return io.EOF
	}
}

func newRows(recordType string, typemap skydb.RecordSchema, rows *sqlx.Rows, err error) (*skydb.Rows, error) {
	if err != nil {
		return nil, err
	}
	rs := newRecordScanner(recordType, typemap, rows)
	return skydb.NewRows(rowsIter{rows, rs}), nil
}

func (db *database) selectQuery(recordType string, typemap skydb.RecordSchema) sq.SelectBuilder {
	columns := make([]string, 0, len(typemap))
	for column := range typemap {
		columns = append(columns, column)
	}

	q := psql.Select()
	for column, fieldType := range typemap {
		if fieldType.Expression != nil {
			sqlOperand, opArgs := toSqlOperand(*fieldType.Expression)
			q = q.Column(sqlOperand+" as "+column, opArgs...)
		} else {
			q = q.Column(`"` + column + `"`)
		}
	}

	q = q.From(db.tableName(recordType)).
		Where("_database_id = ?", db.userID)

	return q
}

// STEP 1 & 2 are obtained by reverse engineering psql \d with -E option
//
// STEP 3: example of getting foreign keys
// SELECT
//     tc.table_name, kcu.column_name,
//     ccu.table_name AS foreign_table_name,
//     ccu.column_name AS foreign_column_name
// FROM
//     information_schema.table_constraints AS tc
//     JOIN information_schema.key_column_usage
//         AS kcu ON tc.constraint_name = kcu.constraint_name
//     JOIN information_schema.constraint_column_usage
//         AS ccu ON ccu.constraint_name = tc.constraint_name
// WHERE constraint_type = 'FOREIGN KEY'
// AND tc.table_schema = 'app__'
// AND tc.table_name = 'note';
func (db *database) remoteColumnTypes(recordType string) (skydb.RecordSchema, error) {
	// STEP 1: Get the oid of the current table
	var oid int
	err := db.Db.QueryRowx(`
SELECT c.oid
FROM pg_catalog.pg_class c
     LEFT JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE c.relname = $1
  AND n.nspname = $2`,
		recordType, db.schemaName()).Scan(&oid)

	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		log.WithFields(log.Fields{
			"schemaName": db.schemaName(),
			"recordType": recordType,
			"err":        err,
		}).Errorln("Failed to query oid of table")
		return nil, err
	}

	// STEP 2: Get column name and data type
	rows, err := db.Db.Queryx(`
SELECT a.attname,
  pg_catalog.format_type(a.atttypid, a.atttypmod)
FROM pg_catalog.pg_attribute a
WHERE a.attrelid = $1 AND a.attnum > 0 AND NOT a.attisdropped`,
		oid)

	if err != nil {
		log.WithFields(log.Fields{
			"schemaName": db.schemaName(),
			"recordType": recordType,
			"oid":        oid,
			"err":        err,
		}).Errorln("Failed to query column and data type")
		return nil, err
	}

	typemap := skydb.RecordSchema{}

	var columnName, pqType string
	for rows.Next() {
		if err := rows.Scan(&columnName, &pqType); err != nil {
			return nil, err
		}

		schema := skydb.FieldType{}
		switch pqType {
		case TypeString:
			schema.Type = skydb.TypeString
		case TypeNumber:
			schema.Type = skydb.TypeNumber
		case TypeTimestamp:
			schema.Type = skydb.TypeDateTime
		case TypeBoolean:
			schema.Type = skydb.TypeBoolean
		case TypeJSON:
			if columnName == "_access" {
				schema.Type = skydb.TypeACL
			} else {
				schema.Type = skydb.TypeJSON
			}
		case TypeLocation:
			schema.Type = skydb.TypeLocation
		default:
			return nil, fmt.Errorf("received unknown data type = %s for column = %s", pqType, columnName)
		}

		typemap[columnName] = schema
	}

	// STEP 3: FOREIGN KEY, assumeing we can only reference _id i.e. "ccu.column_name" = _id
	builder := psql.Select("kcu.column_name", "ccu.table_name").
		From("information_schema.table_constraints AS tc").
		Join("information_schema.key_column_usage AS kcu ON tc.constraint_name = kcu.constraint_name").
		Join("information_schema.constraint_column_usage AS ccu ON ccu.constraint_name = tc.constraint_name").
		Where("constraint_type = 'FOREIGN KEY' AND tc.table_schema = ? AND tc.table_name = ?", db.schemaName(), recordType)

	refs, err := queryWith(db.Db, builder)
	if err != nil {
		log.WithFields(log.Fields{
			"schemaName": db.schemaName(),
			"recordType": recordType,
			"err":        err,
		}).Errorln("Failed to query foreign key information schema")

		return nil, err
	}

	for refs.Next() {
		s := skydb.FieldType{}
		var localColumn, referencedTable string
		if err := refs.Scan(&localColumn, &referencedTable); err != nil {
			log.Debugf("err %v", err)
			return nil, err
		}
		switch referencedTable {
		case "_asset":
			s.Type = skydb.TypeAsset
		default:
			s.Type = skydb.TypeReference
			s.ReferenceType = referencedTable
		}
		typemap[localColumn] = s
	}
	return typemap, nil
}

func (db *database) Extend(recordType string, recordSchema skydb.RecordSchema) error {
	remoteRecordSchema, err := db.remoteColumnTypes(recordType)
	if err != nil {
		return err
	}

	if len(remoteRecordSchema) == 0 {
		if err := db.createTable(recordType); err != nil {
			return fmt.Errorf("failed to create table: %s", err)
		}
	}
	updatingSchema := skydb.RecordSchema{}
	for key, schema := range recordSchema {
		remoteSchema, ok := remoteRecordSchema[key]
		if !ok {
			updatingSchema[key] = schema
		} else if remoteSchema.Type != schema.Type {
			return fmt.Errorf("conflicting schema %s => %s", remoteSchema, schema)
		}

		// same data type, do nothing
	}

	if len(updatingSchema) > 0 {
		stmt := db.addColumnStmt(recordType, updatingSchema)

		log.WithField("stmt", stmt).Debugln("Adding columns to table")
		if _, err := db.Db.Exec(stmt); err != nil {
			return fmt.Errorf("failed to alter table: %s", err)
		}
	}

	return nil
}

func (db *database) createTable(recordType string) (err error) {
	tablename := db.tableName(recordType)

	stmt := createTableStmt(tablename)
	log.WithField("stmt", stmt).Debugln("Creating table")
	_, err = db.Db.Exec(stmt)
	if err != nil {
		return err
	}

	const CreateTriggerStmtFmt = `CREATE TRIGGER trigger_notify_record_change
	AFTER INSERT OR UPDATE OR DELETE ON %s FOR EACH ROW
	EXECUTE PROCEDURE public.notify_record_change();
`
	stmt = fmt.Sprintf(CreateTriggerStmtFmt, tablename)
	log.WithField("stmt", stmt).Debugln("Creating trigger")
	_, err = db.Db.Exec(stmt)

	return err
}

func createTableStmt(tableName string) string {
	return fmt.Sprintf(`
CREATE TABLE %s (
	_id text,
	_database_id text,
	_owner_id text,
	_access jsonb,
	_created_at timestamp without time zone NOT NULL,
	_created_by text,
	_updated_at timestamp without time zone NOT NULL,
	_updated_by text,
	PRIMARY KEY(_id, _database_id, _owner_id),
	UNIQUE (_id)
);
`, tableName)
}

// ALTER TABLE app__.note add collection text;
// ALTER TABLE app__.note
// ADD CONSTRAINT fk_note_collection_collection
// FOREIGN KEY (collection)
// REFERENCES app__.collection(_id);
func (db *database) addColumnStmt(recordType string, recordSchema skydb.RecordSchema) string {
	buf := bytes.Buffer{}
	buf.Write([]byte("ALTER TABLE "))
	buf.WriteString(db.tableName(recordType))
	buf.WriteByte(' ')
	for column, schema := range recordSchema {
		buf.Write([]byte("ADD "))
		buf.WriteString(pq.QuoteIdentifier(column))
		buf.WriteByte(' ')
		buf.WriteString(pqDataType(schema.Type))
		buf.WriteByte(',')
		switch schema.Type {
		case skydb.TypeAsset:
			db.writeForeignKeyConstraint(&buf, column, "_asset", "id")
		case skydb.TypeReference:
			db.writeForeignKeyConstraint(&buf, column, schema.ReferenceType, "_id")
		}
	}

	// remote the last ','
	buf.Truncate(buf.Len() - 1)

	return buf.String()
}

func (db *database) writeForeignKeyConstraint(buf *bytes.Buffer, localCol, referent, remoteCol string) {
	buf.Write([]byte(`ADD CONSTRAINT `))
	buf.WriteString(pq.QuoteIdentifier(fmt.Sprintf(`fk_%s_%s_%s`, localCol, referent, remoteCol)))
	buf.Write([]byte(` FOREIGN KEY (`))
	buf.WriteString(pq.QuoteIdentifier(localCol))
	buf.Write([]byte(`) REFERENCES `))
	buf.WriteString(db.tableName(referent))
	buf.Write([]byte(` (`))
	buf.WriteString(pq.QuoteIdentifier(remoteCol))
	buf.Write([]byte(`),`))
}

func pqDataType(dataType skydb.DataType) string {
	switch dataType {
	default:
		panic(fmt.Sprintf("Unsupported dataType = %s", dataType))
	case skydb.TypeString, skydb.TypeAsset, skydb.TypeReference:
		return TypeString
	case skydb.TypeNumber:
		return TypeNumber
	case skydb.TypeDateTime:
		return TypeTimestamp
	case skydb.TypeBoolean:
		return TypeBoolean
	case skydb.TypeJSON:
		return TypeJSON
	case skydb.TypeLocation:
		return TypeLocation
	}
}