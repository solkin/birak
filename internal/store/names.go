package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

var ErrNameCollision = errors.New("portable namespace collision")

// One spelling per Unicode-normalized, case-folded path component, including
// deletion history. Otherwise a returning replica can overwrite another name
// on APFS/NTFS. Reservations also cover directory components, not just leaves.
func nameParts(name string) ([][2]string, error) {
	if !utf8.ValidString(name) || name == "" {
		return nil, fmt.Errorf("invalid namespace name")
	}
	var out [][2]string
	var folded, original string
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || part == ".." {
			return nil, fmt.Errorf("non-canonical namespace name %q", name)
		}
		key := norm.NFC.String(cases.Fold().String(norm.NFC.String(part)))
		if original != "" {
			original += "/"
			folded += "/"
		}
		original += part
		folded += key
		out = append(out, [2]string{folded, original})
	}
	return out, nil
}

func bindName(tx *sql.Tx, name string) error {
	parts, err := nameParts(name)
	if err != nil {
		return err
	}
	for _, part := range parts {
		if _, err := tx.Exec("INSERT INTO namespace_names(folded,name) VALUES (?,?) ON CONFLICT(folded) DO NOTHING", part[0], part[1]); err != nil {
			return err
		}
		var actual string
		if err := tx.QueryRow("SELECT name FROM namespace_names WHERE folded=?", part[0]).Scan(&actual); err != nil {
			return err
		}
		if actual != part[1] {
			return fmt.Errorf("%w: %q conflicts with reserved spelling %q", ErrNameCollision, part[1], actual)
		}
	}
	return nil
}

func (s *Store) CheckName(name string) error {
	parts, err := nameParts(name)
	if err != nil {
		return err
	}
	for _, part := range parts {
		var actual string
		err := s.db.QueryRow("SELECT name FROM namespace_names WHERE folded=?", part[0]).Scan(&actual)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if err == nil && actual != part[1] {
			return fmt.Errorf("%w: %q conflicts with reserved spelling %q", ErrNameCollision, part[1], actual)
		}
	}
	return nil
}

// Migration is paged and atomic. Ambiguous existing metadata refuses startup
// before any filesystem mutation; an operator can resolve it offline.
func (s *Store) migrateNames() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var migrated string
	err = tx.QueryRow("SELECT value FROM node_meta WHERE key='portable_names_v1'").Scan(&migrated)
	if err == nil {
		return nil
	}
	if err != sql.ErrNoRows {
		return err
	}
	after := ""
	for {
		rows, err := tx.Query("SELECT name FROM files WHERE name>? ORDER BY name LIMIT 1000", after)
		if err != nil {
			return err
		}
		var names []string
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				rows.Close()
				return err
			}
			names = append(names, name)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(names) == 0 {
			break
		}
		for _, name := range names {
			if err := bindName(tx, name); err != nil {
				return err
			}
			after = name
		}
	}
	if _, err := tx.Exec("INSERT INTO node_meta(key,value) VALUES ('portable_names_v1','1')"); err != nil {
		return err
	}
	return tx.Commit()
}
