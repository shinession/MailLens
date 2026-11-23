package outlook

import (
	"errors"
	"fmt"

	ole "github.com/go-ole/go-ole"
	"github.com/go-ole/go-ole/oleutil"
)

// Well-known HRESULTs.
const (
	hrMemberNotFound = 0x80020003 // DISP_E_MEMBERNOTFOUND
	hrMAPINotFound   = 0x8004010F // MAPI_E_NOT_FOUND: the item or folder does not exist
)

// obj is a COM object that has to be released. An object that came out of a VARIANT is released by clearing the
// VARIANT (which holds the reference); the root object has no VARIANT and is released directly.
type obj struct {
	d *ole.IDispatch
	v *ole.VARIANT
}

func (o *obj) release() {
	if o == nil {
		return
	}
	switch {
	case o.v != nil:
		_ = o.v.Clear()
	case o.d != nil:
		o.d.Release()
	}
	o.v, o.d = nil, nil
}

// asObj takes over the reference held by a VARIANT that must contain an object.
func asObj(v *ole.VARIANT, what string) (*obj, error) {
	if v.VT != ole.VT_DISPATCH {
		_ = v.Clear()
		return nil, fmt.Errorf("%s: Outlook returned no object", what)
	}
	d := v.ToIDispatch()
	if d == nil {
		_ = v.Clear()
		return nil, fmt.Errorf("%s: Outlook returned no object", what)
	}
	return &obj{d: d, v: v}, nil
}

// call runs a method. Outlook declares some members (Item, for example) as a method on one collection and as a
// parameterised property on another, so a "member not found" is retried as a property get.
func (o *obj) call(name string, args ...interface{}) (*ole.VARIANT, error) {
	v, err := oleutil.CallMethod(o.d, name, args...)
	if err == nil {
		return v, nil
	}
	if code, _ := hresults(err); code == hrMemberNotFound {
		return oleutil.GetProperty(o.d, name, args...)
	}
	return nil, err
}

// child returns the object a property holds.
func (o *obj) child(name string, args ...interface{}) (*obj, error) {
	v, err := oleutil.GetProperty(o.d, name, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return asObj(v, name)
}

// callChild returns the object a method (or indexed property) returns.
func (o *obj) callChild(name string, args ...interface{}) (*obj, error) {
	v, err := o.call(name, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return asObj(v, name)
}

// at returns the i-th item (1 based) of a collection.
func (o *obj) at(i int) (*obj, error) { return o.callChild("Item", i) }

// value returns the value of a property as a Go value.
func (o *obj) value(name string, args ...interface{}) (interface{}, error) {
	v, err := oleutil.GetProperty(o.d, name, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	defer v.Clear()
	return v.Value(), nil
}

func (o *obj) str(name string) (string, error) {
	val, err := o.value(name)
	if err != nil {
		return "", err
	}
	return asString(val), nil
}

func (o *obj) integer(name string) (int64, error) {
	val, err := o.value(name)
	if err != nil {
		return 0, err
	}
	return asInt64(val), nil
}

func (o *obj) count() (int, error) {
	n, err := o.integer("Count")
	return int(n), err
}

// hresults returns the HRESULT of a failed COM call and, when the call failed with an exception (DISP_E_EXCEPTION,
// which is how Outlook reports its own errors), the error code inside the exception. Both are 0 for other errors.
func hresults(err error) (code, scode uint32) {
	var oe *ole.OleError
	if !errors.As(err, &oe) {
		return 0, 0
	}
	code = uint32(oe.Code())
	if ex, ok := oe.SubError().(ole.EXCEPINFO); ok {
		scode = ex.SCODE()
	}
	return code, scode
}

func isNotFound(err error) bool {
	code, scode := hresults(err)
	return code == hrMAPINotFound || scode == hrMAPINotFound
}
