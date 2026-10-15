package matterclient

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// maxDrainBytes limits how much of a response body decodeJSONArray reads once the array is complete.
const maxDrainBytes = 64 << 10

// decodeJSONArray decodes the JSON array in r one element at a time, calls fn for every element and returns the
// number of elements in the array.
//
// Decoding the array straight into a slice makes encoding/json buffer the entire array (reallocating and copying
// its buffer each time it fills up), and then grow the slice one element at a time. For the large user and channel
// listings that is several times the size of the data itself, and all of it is garbage once the page is cached.
//
// A single element is reused for every call, so fn must not keep the pointer it is given. The element is zeroed
// before each decode as encoding/json merges into existing maps and slices instead of replacing them.
//
// A JSON null is handled as an empty array. An array that ends early is an error rather than a short page.
//
// Once the array is complete the rest of r is read as well, to let net/http put the connection back in its idle pool
// when the body is closed: it throws away the connection of a body that has not been read to the end.
func decodeJSONArray[T any](r io.Reader, fn func(*T)) (int, error) {
	dec := json.NewDecoder(r)

	tok, err := dec.Token()
	if err != nil {
		return 0, unexpectedEOF(err)
	}

	if tok == nil {
		return 0, nil
	}

	if delim, ok := tok.(json.Delim); !ok || delim != '[' {
		return 0, fmt.Errorf("json: cannot decode %v into a list", tok)
	}

	var (
		elem  T
		zero  T
		count int
	)

	for dec.More() {
		elem = zero

		err = dec.Decode(&elem)
		if err != nil {
			return count, unexpectedEOF(err)
		}

		fn(&elem)

		count++
	}

	// dec.More also reports false when the input ends early, so the closing bracket has to be read to tell the two apart.
	_, err = dec.Token()
	if err != nil {
		return count, unexpectedEOF(err)
	}

	_, _ = io.Copy(io.Discard, io.LimitReader(r, maxDrainBytes))

	return count, nil
}

func unexpectedEOF(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}

	return err
}
