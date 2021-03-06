package upsert

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"reflect"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
)

const (
	Inserted = iota
	Updated
	NoChange
)

func statusToText(status int) string {
	switch status {
	case Inserted:
		return "inserted"
	case Updated:
		return "updated"
	case NoChange:
		return "no change"
	}

	return "invalid status"
}

var (
	ErrNoIDReturned = errors.New("no id returned")

	// LongQuery will log long queries if set to a non-zero time
	LongQuery time.Duration
	Debug     = false
)

// Upserter is an interface specific to sqlx and PostgreSQL that can save a
// single row of data via Upsert(), Update() or Insert(). It doesn't try to
// know anything about relationships between tables. The behavior of Upserter
// depends on three struct tags.
//
//  * db: As with sqlx, this tag is the database column name for the field.
//     If db is not defined, the default is the lowercase value of the field
//     name.
//
//  * upsert: This may either be "key" or "omit". If it's "key", the
//     field/column is part of the where clause when attempting to update
//     an existing column. If it's "omit", the field is ignored completely.
//     By default, the field is considered a non-key value that should be
//     updated/set in the db.
//
//  * upsert_value: This is the placeholder for the value of the field for
//     use by sqlx.NamedExec(). By default, this is :column_name and typically
//     doesn't need to be changed. However, if the field needs to be
//     transformed by an SQL function before storing in the database,
//     this tag can be set. For example, if you had "lat" and "lon" columns,
//     you wouldn't want to store them in the db. Instead you'd want a
//     "location" column tagged with `upsert_value:"ll_to_earth(:lat, :lon)`
//
type Upserter interface {
	// Table returns table name we should save to
	Table() string
}

type columnSpec struct {
	name  string
	value string
}

func newColumnSpec(fieldName string, tag reflect.StructTag) columnSpec {
	cs := columnSpec{}

	// The name of the column is either the value of the "db" struct tag
	// or a lowercase version of the field name.
	dbTag := tag.Get("db")
	if len(dbTag) > 0 {
		cs.name = dbTag
	} else {
		cs.name = strings.ToLower(fieldName)
	}

	// The value placeholder of the column is typically just ":column_name"
	// but can be overriden with upsert_value.
	val := tag.Get("upsert_value")
	if len(val) > 0 {
		cs.value = val
	} else {
		cs.value = ":" + cs.name
	}

	return cs
}

// updateColumns returns the fields that are read from the struct and set
// on upserting in the db. Typically this should include everything except the
// key fields and any composite (array, nested struct) types or any
// field that doesn't map directly into a db column. Tag a field with
// `upsert:"omit"` to explicitly exclude from this list.
func updateColumns(u interface{}) (columns []columnSpec) {
	ut := reflect.TypeOf(u)

	if ut.Kind() == reflect.Ptr {
		ut = ut.Elem()
	}

	if ut.Kind() != reflect.Struct {
		return
	}

	for i := 0; i < ut.NumField(); i++ {
		field := ut.Field(i)
		tag := field.Tag

		// Include any column that isn't tagged with upsert:omit
		if !strings.Contains(tag.Get("upsert"), "omit") {
			columns = append(columns, newColumnSpec(field.Name, tag))
		}
	}

	return
}

func updateValueString(u interface{}) string {
	b := bytes.NewBuffer(nil)

	ut := reflect.TypeOf(u)

	if ut.Kind() == reflect.Ptr {
		ut = ut.Elem()
	}

	if ut.Kind() != reflect.Struct {
		return ""
	}

	val := reflect.ValueOf(u).Elem()

	for i := 0; i < ut.NumField(); i++ {
		field := ut.Field(i)
		tag := field.Tag

		// Include any column that isn't tagged with upsert:omit and doesn't
		// have an upsert_value
		if !strings.Contains(tag.Get("upsert"), "omit") && len(tag.Get("upsert_value")) < 1 {
			x := val.Field(i).Interface()
			switch v := x.(type) {
			case time.Time:
				fmt.Fprintf(b, "%v ", v.Unix())
			default:
				fmt.Fprintf(b, "%v ", x)
			}
		}
	}

	return b.String()
}

// uniqueKeyColumns returns the fields of the struct that together are
// naturally unique. For example, an md5 hash of the content. Or a
// foreign key plus an internal value. This is used in where clause
// when trying to find existing rows. Tag a field with `"upsert:"key"`
// to include in the unique key.
func uniqueKeyColumns(u interface{}) (columns []columnSpec) {
	ut := reflect.TypeOf(u)

	if ut.Kind() == reflect.Ptr {
		ut = ut.Elem()
	}

	if ut.Kind() != reflect.Struct {
		return
	}

	for i := 0; i < ut.NumField(); i++ {
		field := ut.Field(i)
		tag := field.Tag
		// Check if upsert tag contains "key". This wouldn't work
		// if possible options were substrings of one another. For a
		// better implementation, look at src/encoding/json/tags.go
		if strings.Contains(tag.Get("upsert"), "key") {
			columns = append(columns, newColumnSpec(field.Name, tag))
		}
	}

	return
}

// set returns a string like "SET "col1" = :col1, "col2" = :col2" for
// use with sqlx.NamedExec() and friends.
func set(u Upserter) string {
	cols := updateColumns(u)
	n := len(cols)

	b := bytes.Buffer{}

	b.WriteString("SET ")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, `"%s" = %s`, cols[i].name, cols[i].value)

		// If we are not at the last value, add a comma
		if i < n-1 {
			b.WriteRune(',')
		}
	}

	return b.String()
}

// values returns a string like `("col1", "col2") VALUES(:col1, :col2)`
// for use with sqlx.NamedExec() etc.
func values(u interface{}) string {
	cols := updateColumns(u)
	n := len(cols)

	b := bytes.Buffer{}

	b.WriteRune('(')
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, `"%s"`, cols[i].name)

		// If we are not at the last value, add a comma
		if i < n-1 {
			b.WriteRune(',')
		}
	}
	b.WriteRune(')')

	b.WriteString("VALUES (")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, `%s`, cols[i].value)

		// If we are not at the last value, add a comma
		if i < n-1 {
			b.WriteRune(',')
		}
	}
	b.WriteRune(')')

	return b.String()
}

// where returns an SQL where clause with all the key columns of
// this Upserter
func where(u Upserter) string {
	keycols := uniqueKeyColumns(u)
	b := bytes.Buffer{}
	n := len(keycols)

	fmt.Fprintf(&b, "WHERE ")

	for i := 0; i < n; i++ {
		// If we need to support NULLs here, the best option may be
		// something like (x = y OR (x is null and y is null))
		// rather than "IS NOT DISTINCT FROM" which doesn't use indexes
		// it seems
		fmt.Fprintf(&b, `%s = %s`, keycols[i].name, keycols[i].value)

		if i < n-1 {
			fmt.Fprint(&b, " AND ")
		}
	}

	return b.String()
}

// updateSQL returns a full SQL command to update this Upserter u
func updateSQL(u Upserter) string {
	q := fmt.Sprintf(`UPDATE "%s" %s %s RETURNING *`,
		u.Table(), set(u), where(u))

	return q
}

// insertSQL returns a full SQL command to insert this Upserter u
func insertSQL(u Upserter) string {
	q := fmt.Sprintf(`INSERT INTO "%s" %s RETURNING *`, u.Table(), values(u))

	return q
}

// getSQL returns a full SQL command to retrieve this Upserter u
func getSQL(u Upserter) string {
	q := fmt.Sprintf(`SELECT * FROM %s %s`, u.Table(), where(u))

	return q
}

func Update(ext sqlx.Ext, u Upserter) (status int, err error) {
	q := updateSQL(u)

	if LongQuery > time.Duration(0) {
		t1 := time.Now()
		defer func() {
			t2 := time.Now()
			if t2.Sub(t1) > LongQuery {
				log.Println(t2.Sub(t1), q, u)
			}
		}()
	}

	otherPtr := reflect.New(reflect.TypeOf(u).Elem())
	other := reflect.Indirect(otherPtr)
	otherInterface := other.Addr().Interface()

	rows, err := sqlx.NamedQuery(ext, getSQL(u), u)
	if err != nil {
		log.Println("error getting", err)
		return
	}

	if rows.Next() {
		err = rows.StructScan(otherInterface)
		if err != nil {
			log.Println("error scanning", err)
			return
		}

		otherKeys := values(otherInterface)
		uKeys := values(u)

		otherValues := updateValueString(otherInterface)
		uValues := updateValueString(u)

		if Debug {
			log.Println(otherKeys)
			log.Println(uKeys)
			log.Println(otherValues)
			log.Println(uValues)
		}

		if otherKeys == uKeys && otherValues == uValues {
			status = NoChange
			rows.Close()
			return
		}
	}
	rows.Close()

	status = Updated

	// Try to update an existing row
	rows, err = sqlx.NamedQuery(ext, q, u)
	if err != nil {
		log.Println(updateSQL(u), err)
		return
	}
	defer rows.Close()

	if rows.Next() {
		err = rows.StructScan(u)
		if err != nil {
			log.Println(err)
		}
	} else {
		// We could not find anything to update.
		err = ErrNoIDReturned
		return
	}

	return
}

// Insert takes either an sqlx.DB or sqlx.Tx as ext, along with a value
// that implements the Upserter() interface. We attempt to insert it
// and set its primary key id value.
func Insert(ext sqlx.Ext, u Upserter) (err error) {
	q := insertSQL(u)

	if LongQuery > time.Duration(0) {
		t1 := time.Now()
		defer func() {
			t2 := time.Now()
			if t2.Sub(t1) > LongQuery {
				log.Println(t2.Sub(t1), q, u)
			}
		}()
	}

	// Try to insert a row
	rows, err := sqlx.NamedQuery(ext, q, u)
	if err != nil {
		log.Println(err)
		return
	}
	defer rows.Close()

	if rows.Next() {
		err = rows.StructScan(u)
		if err != nil {
			log.Println(err)
		}
	} else {
		// No rows were returned but no SQL error. Weird, return generic
		// error.
		err = ErrNoIDReturned
		return
	}

	return
}

// Upsert takes either an sqlx.DB or sqlx.Tx as ext, along with a value
// that implements the Upserter() interface. We attempt to insert/update it
// and set the new primary key id if that succeeds. inserted returns true
// if a new row was inserted. The client is responsible for wrapping
// in a transaction when needed. This can be used when running a transaction
// at a higher level (upserting multiple items).
func Upsert(ext sqlx.Ext, u Upserter) (status int, err error) {
	defer func() {
		if Debug {
			log.Println(statusToText(status), u)
		}
	}()

	// Try to update, return immediately if succcesful
	status, err = Update(ext, u)
	if err == nil {
		return
	}

	// Can't update? Try insert
	err = Insert(ext, u)
	if err != nil {
		log.Println(err)
		return
	}

	status = Inserted

	return
}

// UpsertTx takes only an sqlx.DB and wraps the upsert attempt into a
// a transaction.
func UpsertTx(db *sqlx.DB, u Upserter) (status int, err error) {
	defer func() {
		if Debug {
			log.Println(statusToText(status), u)
		}
	}()

	tx, err := db.Beginx()
	if err != nil {
		log.Println("can't start transaction", err)
		return
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		} else {
			tx.Commit()
		}
	}()

	// Try to update
	status, err = Update(tx, u)

	// If we have a nil error, we successfully updated. If we have
	// an err other than ErrNoIDReturned, we couldn't update for an
	// unexpected reason. In either case return.
	if err != ErrNoIDReturned {
		return
	}

	// No ID returned in the update? Try insert
	err = Insert(tx, u)
	if err != nil {
		log.Println(err)
		return
	}

	status = Inserted

	if Debug {
		log.Println(statusToText(status), u)
	}

	return
}

func Delete(ext sqlx.Ext, u Upserter) (err error) {
	q := fmt.Sprintf(`DELETE FROM "%s" %s`,
		u.Table(), where(u))
	_, err = sqlx.NamedExec(ext, q, u)

	if err != nil {
		log.Println("can't delete", err)
		return
	}

	return
}
