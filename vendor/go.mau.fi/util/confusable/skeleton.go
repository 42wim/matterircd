// Copyright (c) 2024 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package confusable implements confusable detection according to UTS #39.
package confusable

import (
	"bytes"
	"crypto/sha256"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

//go:generate go run ./generate.go

// Skeleton is the skeleton function defined in UTS #39.
//
// If two strings have the same skeleton, they're considered confusable.
// The skeleton strings are not intended for displaying to users.
//
// See https://www.unicode.org/reports/tr39/#Confusable_Detection for more info.
func Skeleton(input string) string {
	input = norm.NFD.String(input)
	var builder strings.Builder
	builder.Grow(len(input))
	for _, r := range input {
		if !unicode.IsGraphic(r) && r != 0xfffd {
			continue
		}
		repl := GetReplacement(r)
		if repl != "" {
			builder.WriteString(repl)
		} else {
			builder.WriteRune(r)
		}
	}
	return norm.NFD.String(builder.String())
}

// SkeletonBytes is the same as Skeleton, but it returns the skeleton as a byte slice.
func SkeletonBytes(input string) []byte {
	input = norm.NFD.String(input)
	var builder bytes.Buffer
	builder.Grow(len(input))
	for _, r := range input {
		if !unicode.IsGraphic(r) && r != 0xfffd {
			continue
		}
		repl := GetReplacement(r)
		if repl != "" {
			builder.WriteString(repl)
		} else {
			builder.WriteRune(r)
		}
	}
	return norm.NFD.Bytes(builder.Bytes())
}

// SkeletonHash returns the sha256 hash of the skeleton of the string.
func SkeletonHash(input string) [32]byte {
	return sha256.Sum256(SkeletonBytes(input))
}

// Confusable checks if two strings are confusable.
func Confusable(x, y string) bool {
	return Skeleton(x) == Skeleton(y)
}
