/*
Copyright 2019 The Vitess Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package mysql

import (
	"vitess.io/vitess/go/mysql/sqlerror"
	"vitess.io/vitess/go/sqltypes"
	"vitess.io/vitess/go/vt/sqlparser"

	querypb "vitess.io/vitess/go/vt/proto/query"
)

// This file contains the methods needed to execute streaming queries.

// ExecuteStreamFetch starts a streaming query.  Fields(), FetchNext() and
// CloseResult() can be called once this is successful.
// Returns a SQLError.
func (c *Conn) ExecuteStreamFetch(query string) (err error) {
	defer func() {
		if err != nil {
			if sqlerr, ok := err.(*sqlerror.SQLError); ok {
				sqlerr.Query = sqlparser.TruncateQuery(query, c.truncateErrLen)
			}
		}
	}()

	// Sanity check.
	if c.fields != nil {
		return sqlerror.NewSQLError(sqlerror.CRCommandsOutOfSync, sqlerror.SSUnknownSQLState, "streaming query already in progress")
	}

	// Send the query as a COM_QUERY packet.
	if err := c.WriteComQuery(query); err != nil {
		return err
	}

	// Get the result.
	colNumber, _, err := c.readComQueryResponse()
	if err != nil {
		return err
	}
	if colNumber == 0 {
		// OK packet, means no results. Save an empty Fields array.
		c.fields = make([]*querypb.Field, 0)
		return nil
	}

	// Read the fields, save them.
	fields := make([]querypb.Field, colNumber)
	fieldsPointers := make([]*querypb.Field, colNumber)

	// Read column headers. One packet per column.
	// Build the fields.
	for i := 0; i < colNumber; i++ {
		fieldsPointers[i] = &fields[i]
		if err := c.readColumnDefinition(fieldsPointers[i], i); err != nil {
			return err
		}
	}

	// Read the EOF after the fields if necessary.
	if c.Capabilities&CapabilityClientDeprecateEOF == 0 {
		// EOF is only present here if it's not deprecated.
		data, err := c.readEphemeralPacket()
		if err != nil {
			return sqlerror.NewSQLError(sqlerror.CRServerLost, sqlerror.SSUnknownSQLState, "%v", err)
		}
		defer c.recycleReadPacket()
		if c.isEOFPacket(data) {
			// This is what we expect.
			// Warnings and status flags are ignored.
			// goto: end
		} else if isErrorPacket(data) {
			return ParseErrorPacket(data)
		} else {
			return sqlerror.NewSQLError(sqlerror.CRCommandsOutOfSync, sqlerror.SSUnknownSQLState, "unexpected packet after fields: %v", data)
		}
	}

	c.fields = fieldsPointers
	return nil
}

// Fields returns the fields for an ongoing streaming query.
func (c *Conn) Fields() ([]*querypb.Field, error) {
	if c.fields == nil {
		return nil, sqlerror.NewSQLError(sqlerror.CRCommandsOutOfSync, sqlerror.SSUnknownSQLState, "no streaming query in progress")
	}
	if len(c.fields) == 0 {
		// The query returned an empty field list.
		return nil, nil
	}
	return c.fields, nil
}

// FetchNext returns the next result for an ongoing streaming query.
// It returns (nil, nil) if there is nothing more to read.
func (c *Conn) FetchNext(in []sqltypes.Value) ([]sqltypes.Value, error) {
	if c.fields == nil {
		// We are already done, and the result was closed.
		return nil, sqlerror.NewSQLError(sqlerror.CRCommandsOutOfSync, sqlerror.SSUnknownSQLState, "no streaming query in progress")
	}

	if len(c.fields) == 0 {
		// We received no fields, so there is no data.
		return nil, nil
	}

	data, err := c.ReadPacket()
	if err != nil {
		return nil, err
	}

	if c.isEOFPacket(data) {
		// Warnings and status flags are ignored.
		c.fields = nil
		return nil, nil
	} else if isErrorPacket(data) {
		// Error packet.
		return nil, ParseErrorPacket(data)
	}

	// Regular row.
	return c.parseRow(data, c.fields, readLenEncStringAsBytes, in)
}

// CloseResult can be used to terminate a streaming query
// early. It just drains the remaining values.
func (c *Conn) CloseResult() {
	row := make([]sqltypes.Value, 0, len(c.fields))
	for c.fields != nil {
		rows, err := c.FetchNext(row[:0])
		if err != nil || rows == nil {
			// We either got an error, or got the last result.
			c.fields = nil
		}
	}
}
