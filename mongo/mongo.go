// Copyright (C) MongoDB, Inc. 2017-present.
//
// Licensed under the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License. You may obtain
// a copy of the License at http://www.apache.org/licenses/LICENSE-2.0

package mongo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"reflect"
	"strconv"
	"strings"

	"go.mongodb.org/mongo-driver/v2/internal/codecutil"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/x/bsonx/bsoncore"

	"go.mongodb.org/mongo-driver/v2/bson"
)

var defaultRegistry = bson.NewRegistry()

// Dialer is used to make network connections.
type Dialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

// Pipeline is a type that makes creating aggregation pipelines easier. It is a
// helper and is intended for serializing to BSON.
//
// Example usage:
//
//	mongo.Pipeline{
//		{{"$group", bson.D{{"_id", "$state"}, {"totalPop", bson.D{{"$sum", "$pop"}}}}}},
//		{{"$match", bson.D{{"totalPop", bson.D{{"$gte", 10*1000*1000}}}}}},
//	}
type Pipeline []bson.D

// getEncoder takes a writer, BSON options, and a BSON registry and returns a properly configured
// bson.Encoder that writes to the given writer.
func getEncoder(
	w io.Writer,
	opts *options.BSONOptions,
	reg *bson.Registry,
) *bson.Encoder {
	vw := bson.NewDocumentWriter(w)
	enc := bson.NewEncoder(vw)

	if opts != nil {
		if opts.ErrorOnInlineDuplicates {
			enc.ErrorOnInlineDuplicates()
		}
		if opts.IntMinSize {
			enc.IntMinSize()
		}
		if opts.NilByteSliceAsEmpty {
			enc.NilByteSliceAsEmpty()
		}
		if opts.NilMapAsEmpty {
			enc.NilMapAsEmpty()
		}
		if opts.NilSliceAsEmpty {
			enc.NilSliceAsEmpty()
		}
		if opts.OmitZeroStruct {
			enc.OmitZeroStruct()
		}
		if opts.OmitEmpty {
			enc.OmitEmpty()
		}
		if opts.StringifyMapKeysWithFmt {
			enc.StringifyMapKeysWithFmt()
		}
		if opts.UseJSONStructTags {
			enc.UseJSONStructTags()
		}
	}

	if reg != nil {
		enc.SetRegistry(reg)
	}

	return enc
}

// newEncoderFn will return a function for constructing an encoder based on the
// provided codec options.
func newEncoderFn(opts *options.BSONOptions, registry *bson.Registry) codecutil.EncoderFn {
	return func(w io.Writer) *bson.Encoder {
		return getEncoder(w, opts, registry)
	}
}

// marshal marshals the given value as a BSON document. Byte slices are always converted to a
// bson.Raw before marshaling.
//
// If bsonOpts and registry are specified, the encoder is configured with the requested behaviors.
// If they are nil, the default behaviors are used.
func marshal(
	val interface{},
	bsonOpts *options.BSONOptions,
	registry *bson.Registry,
) (bsoncore.Document, error) {
	if registry == nil {
		registry = defaultRegistry
	}
	if val == nil {
		return nil, ErrNilDocument
	}
	if bs, ok := val.([]byte); ok {
		// Slight optimization so we'll just use MarshalBSON and not go through the codec machinery.
		val = bson.Raw(bs)
	}

	buf := new(bytes.Buffer)
	enc := getEncoder(buf, bsonOpts, registry)
	err := enc.Encode(val)
	if err != nil {
		return nil, MarshalError{Value: val, Err: err}
	}

	return buf.Bytes(), nil
}

// ensureID inserts the given ObjectID as an element named "_id" at the
// beginning of the given BSON document if there is not an "_id" already.
// If the given ObjectID is bson.NilObjectID, a new object ID will be
// generated with time.Now().
//
// If there is already an element named "_id", the document is not modified. It
// returns the resulting document and the decoded Go value of the "_id" element.
func ensureID(
	doc bsoncore.Document,
	oid bson.ObjectID,
	bsonOpts *options.BSONOptions,
	reg *bson.Registry,
) (bsoncore.Document, interface{}, error) {
	if reg == nil {
		reg = defaultRegistry
	}

	// Try to find the "_id" element. If it exists, try to unmarshal just the
	// "_id" field as an interface{} and return it along with the unmodified
	// BSON document.
	if _, err := doc.LookupErr("_id"); err == nil {
		var id struct {
			ID interface{} `bson:"_id"`
		}
		dec := getDecoder(doc, bsonOpts, reg)
		err = dec.Decode(&id)
		if err != nil {
			return nil, nil, fmt.Errorf("error unmarshaling BSON document: %w", err)
		}

		return doc, id.ID, nil
	}

	// We couldn't find an "_id" element, so add one with the value of the
	// provided ObjectID.

	olddoc := doc

	// Reserve an extra 17 bytes for the "_id" field we're about to add:
	// type (1) + "_id" (3) + terminator (1) + object ID (12)
	const extraSpace = 17
	doc = make(bsoncore.Document, 0, len(olddoc)+extraSpace)
	_, doc = bsoncore.ReserveLength(doc)
	if oid.IsZero() {
		oid = bson.NewObjectID()
	}
	doc = bsoncore.AppendObjectIDElement(doc, "_id", oid)

	// Remove and re-write the BSON document length header.
	const int32Len = 4
	doc = append(doc, olddoc[int32Len:]...)
	doc = bsoncore.UpdateLength(doc, 0, int32(len(doc)))

	return doc, oid, nil
}

func ensureDollarKey(doc bsoncore.Document) error {
	firstElem, err := doc.IndexErr(0)
	if err != nil {
		return errors.New("update document must have at least one element")
	}

	if !strings.HasPrefix(firstElem.Key(), "$") {
		return errors.New("update document must contain key beginning with '$'")
	}
	return nil
}

func ensureNoDollarKey(doc bsoncore.Document) error {
	if elem, err := doc.IndexErr(0); err == nil && strings.HasPrefix(elem.Key(), "$") {
		return errors.New("replacement document cannot contain keys beginning with '$'")
	}

	return nil
}

func marshalAggregatePipeline(
	pipeline interface{},
	bsonOpts *options.BSONOptions,
	registry *bson.Registry,
) (bsoncore.Document, bool, error) {
	switch t := pipeline.(type) {
	case bson.ValueMarshaler:
		btype, val, err := t.MarshalBSONValue()
		if err != nil {
			return nil, false, err
		}
		if typ := bson.Type(btype); typ != bson.TypeArray {
			return nil, false, fmt.Errorf("ValueMarshaler returned a %v, but was expecting %v", typ, bson.TypeArray)
		}

		var hasOutputStage bool
		pipelineDoc := bsoncore.Document(val)
		values, _ := pipelineDoc.Values()
		if pipelineLen := len(values); pipelineLen > 0 {
			if finalDoc, ok := values[pipelineLen-1].DocumentOK(); ok {
				if elem, err := finalDoc.IndexErr(0); err == nil && (elem.Key() == "$out" || elem.Key() == "$merge") {
					hasOutputStage = true
				}
			}
		}

		return pipelineDoc, hasOutputStage, nil
	default:
		val := reflect.ValueOf(t)
		if !val.IsValid() || (val.Kind() != reflect.Slice && val.Kind() != reflect.Array) {
			return nil, false, fmt.Errorf("can only marshal slices and arrays into aggregation pipelines, but got %v", val.Kind())
		}

		var hasOutputStage bool
		valLen := val.Len()

		switch t := pipeline.(type) {
		// Explicitly forbid non-empty pipelines that are semantically single documents
		// and are implemented as slices.
		case bson.D, bson.Raw, bsoncore.Document:
			if valLen > 0 {
				return nil, false,
					fmt.Errorf("%T is not an allowed pipeline type as it represents a single document. Use bson.A or mongo.Pipeline instead", t)
			}
		// bsoncore.Arrays do not need to be marshaled. Only check validity and presence of output stage.
		case bsoncore.Array:
			if err := t.Validate(); err != nil {
				return nil, false, err
			}

			values, err := t.Values()
			if err != nil {
				return nil, false, err
			}

			numVals := len(values)
			if numVals == 0 {
				return bsoncore.Document(t), false, nil
			}

			// If not empty, check if first value of the last stage is $out or $merge.
			if lastStage, ok := values[numVals-1].DocumentOK(); ok {
				if elem, err := lastStage.IndexErr(0); err == nil && (elem.Key() == "$out" || elem.Key() == "$merge") {
					hasOutputStage = true
				}
			}
			return bsoncore.Document(t), hasOutputStage, nil
		}

		aidx, arr := bsoncore.AppendArrayStart(nil)
		for idx := 0; idx < valLen; idx++ {
			doc, err := marshal(val.Index(idx).Interface(), bsonOpts, registry)
			if err != nil {
				return nil, false, err
			}

			if idx == valLen-1 {
				if elem, err := doc.IndexErr(0); err == nil && (elem.Key() == "$out" || elem.Key() == "$merge") {
					hasOutputStage = true
				}
			}
			arr = bsoncore.AppendDocumentElement(arr, strconv.Itoa(idx), doc)
		}
		arr, _ = bsoncore.AppendArrayEnd(arr, aidx)
		return arr, hasOutputStage, nil
	}
}

func marshalUpdateValue(
	update interface{},
	bsonOpts *options.BSONOptions,
	registry *bson.Registry,
	dollarKeysAllowed bool,
) (bsoncore.Value, error) {
	documentCheckerFunc := ensureDollarKey
	if !dollarKeysAllowed {
		documentCheckerFunc = ensureNoDollarKey
	}

	var u bsoncore.Value
	var err error
	switch t := update.(type) {
	case nil:
		return u, ErrNilDocument
	case bson.D:
		u.Type = bsoncore.TypeEmbeddedDocument
		u.Data, err = marshal(update, bsonOpts, registry)
		if err != nil {
			return u, err
		}

		return u, documentCheckerFunc(u.Data)
	case bson.Raw:
		u.Type = bsoncore.TypeEmbeddedDocument
		u.Data = t
		return u, documentCheckerFunc(u.Data)
	case bsoncore.Document:
		u.Type = bsoncore.TypeEmbeddedDocument
		u.Data = t
		return u, documentCheckerFunc(u.Data)
	case []byte:
		u.Type = bsoncore.TypeEmbeddedDocument
		u.Data = t
		return u, documentCheckerFunc(u.Data)
	case bson.Marshaler:
		u.Type = bsoncore.TypeEmbeddedDocument
		u.Data, err = t.MarshalBSON()
		if err != nil {
			return u, err
		}

		return u, documentCheckerFunc(u.Data)
	case bson.ValueMarshaler:
		tt, data, err := t.MarshalBSONValue()
		u.Type = bsoncore.Type(tt)
		u.Data = data
		if err != nil {
			return u, err
		}
		if u.Type != bsoncore.TypeArray && u.Type != bsoncore.TypeEmbeddedDocument {
			return u, fmt.Errorf("ValueMarshaler returned a %v, but was expecting %v or %v", u.Type, bsoncore.TypeArray, bsoncore.TypeEmbeddedDocument)
		}
		return u, err
	default:
		val := reflect.ValueOf(t)
		if !val.IsValid() {
			return u, fmt.Errorf("can only marshal slices and arrays into update pipelines, but got %v", val.Kind())
		}
		if val.Kind() != reflect.Slice && val.Kind() != reflect.Array {
			u.Type = bsoncore.TypeEmbeddedDocument
			u.Data, err = marshal(update, bsonOpts, registry)
			if err != nil {
				return u, err
			}

			return u, documentCheckerFunc(u.Data)
		}

		u.Type = bsoncore.TypeArray
		aidx, arr := bsoncore.AppendArrayStart(nil)
		valLen := val.Len()
		for idx := 0; idx < valLen; idx++ {
			doc, err := marshal(val.Index(idx).Interface(), bsonOpts, registry)
			if err != nil {
				return u, err
			}

			if err := documentCheckerFunc(doc); err != nil {
				return u, err
			}

			arr = bsoncore.AppendDocumentElement(arr, strconv.Itoa(idx), doc)
		}
		u.Data, _ = bsoncore.AppendArrayEnd(arr, aidx)
		return u, err
	}
}

func marshalValue(
	val interface{},
	bsonOpts *options.BSONOptions,
	registry *bson.Registry,
) (bsoncore.Value, error) {
	return codecutil.MarshalValue(val, newEncoderFn(bsonOpts, registry))
}

// Build the aggregation pipeline for the CountDocument command.
func countDocumentsAggregatePipeline(
	filter interface{},
	encOpts *options.BSONOptions,
	registry *bson.Registry,
	args *options.CountOptions,
) (bsoncore.Document, error) {
	filterDoc, err := marshal(filter, encOpts, registry)
	if err != nil {
		return nil, err
	}

	aidx, arr := bsoncore.AppendArrayStart(nil)
	didx, arr := bsoncore.AppendDocumentElementStart(arr, strconv.Itoa(0))
	arr = bsoncore.AppendDocumentElement(arr, "$match", filterDoc)
	arr, _ = bsoncore.AppendDocumentEnd(arr, didx)

	index := 1
	if args != nil {
		if args.Skip != nil {
			didx, arr = bsoncore.AppendDocumentElementStart(arr, strconv.Itoa(index))
			arr = bsoncore.AppendInt64Element(arr, "$skip", *args.Skip)
			arr, _ = bsoncore.AppendDocumentEnd(arr, didx)
			index++
		}
		if args.Limit != nil {
			didx, arr = bsoncore.AppendDocumentElementStart(arr, strconv.Itoa(index))
			arr = bsoncore.AppendInt64Element(arr, "$limit", *args.Limit)
			arr, _ = bsoncore.AppendDocumentEnd(arr, didx)
			index++
		}
	}

	didx, arr = bsoncore.AppendDocumentElementStart(arr, strconv.Itoa(index))
	iidx, arr := bsoncore.AppendDocumentElementStart(arr, "$group")
	arr = bsoncore.AppendInt32Element(arr, "_id", 1)
	iiidx, arr := bsoncore.AppendDocumentElementStart(arr, "n")
	arr = bsoncore.AppendInt32Element(arr, "$sum", 1)
	arr, _ = bsoncore.AppendDocumentEnd(arr, iiidx)
	arr, _ = bsoncore.AppendDocumentEnd(arr, iidx)
	arr, _ = bsoncore.AppendDocumentEnd(arr, didx)

	return bsoncore.AppendArrayEnd(arr, aidx)
}
