package goxstream

// #cgo CFLAGS: -I./include -fPIC
// #cgo LDFLAGS: -lclntsh
/* #
#include "xstrm.c"
*/
import "C"
import (
	"fmt"
	"github.com/chai2010/cgo"
	"github.com/yjhatfdu/goxstream/scn"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/unicode"
	"log"
	"reflect"
	"time"
	"unicode/utf16"
	"unsafe"
)

var decoders = map[int]func(b []byte) (string, error){}

func init() {
	decoders[2000] = func(b []byte) (string, error) {
		buf := make([]uint16, len(b)/2)
		for i := range buf {
			buf[i] = uint16(b[2*i]) + uint16(b[2*i+1])<<8
		}
		return string(utf16.Decode(buf)), nil
	}
	gbkDecoder := simplifiedchinese.GBK.NewDecoder()
	decoders[852] = func(b []byte) (string, error) {
		s, err := gbkDecoder.Bytes(b)
		return string(s), err
	}
	decoders[0] = func(b []byte) (string, error) {
		return string(b), nil
	}
	utf8Decoder := unicode.UTF8.NewDecoder()
	decoders[873] = func(b []byte) (string, error) {
		s, err := utf8Decoder.Bytes(b)
		return string(s), err
	}
}

func toOciStr(s string) (*C.uchar, C.uint, func()) {
	uchars := cgo.NewUInt8N(len(s))
	l := uint32(len(s))
	cs := uchars.Slice(len(s))
	copy(cs, s)
	return (*C.uchar)(uchars), C.uint(l), func() {
		uchars.Free()
	}
}

type XStreamConn struct {
	ocip  *C.struct_oci
	csid  int
	ncsid int
}

func Open(username, password, dbname, servername string) (*XStreamConn, error) {
	var info C.struct_conn_info
	usernames, usernamel, free := toOciStr(username)
	defer free()
	info.user = usernames
	info.userlen = usernamel
	psws, pswl, free2 := toOciStr(password)
	defer free2()
	info.passw = psws
	info.passwlen = pswl
	dbs, dbl, free3 := toOciStr(dbname)
	defer free3()
	info.dbname = dbs
	info.dbnamelen = dbl
	svrs, svrl, free4 := toOciStr(servername)
	defer free4()
	info.svrnm = svrs
	info.svrnmlen = svrl
	var oci *C.struct_oci
	var char_csid, nchar_csid C.ushort
	C.get_db_charsets(&info, &char_csid, &nchar_csid)
	C.connect_db(&info, &oci, char_csid, nchar_csid)
	r := C.attach0(oci, &info, C.int(1))
	if int(r) != 0 {
		errstr, errcode, err := getErrorEnc(oci.errp, int(char_csid))
		if err != nil {
			return nil, fmt.Errorf("failed to parse oci error after calling Open function failed: %s", err.Error())
		}
		return nil, fmt.Errorf("attach to XStream server specified in connection info failed, code:%d, %s", errcode, errstr)
	}

	return &XStreamConn{
		ocip:  oci,
		csid:  int(char_csid),
		ncsid: int(nchar_csid),
	}, nil
}

func (x *XStreamConn) Close() error {
	C.detach(x.ocip)
	C.disconnect_db(x.ocip)
	C.free(unsafe.Pointer(x.ocip))
	return nil
}

func ociNumberToInt(errp *C.OCIError, number *C.OCINumber) int64 {
	var i int64
	C.OCINumberToInt(errp, number, 8, C.OCI_NUMBER_SIGNED, unsafe.Pointer(&i))
	return i
}

func ociNumberFromInt(errp *C.OCIError, i int64) *C.OCINumber {
	var n C.OCINumber
	C.OCINumberFromInt(errp, unsafe.Pointer(&i), 8, C.OCI_NUMBER_SIGNED, &n)
	return &n
}

func (x *XStreamConn) SetSCNLwm(s scn.SCN) error {
	pos, posl := scn2pos(x.ocip, s)
	defer C.free(unsafe.Pointer(pos))
	status := C.OCIXStreamOutProcessedLWMSet(x.ocip.svcp, x.ocip.errp, pos, posl, C.OCI_DEFAULT)
	if status == C.OCI_ERROR {
		errstr, errcode := getError(x.ocip.errp)
		return fmt.Errorf("set position lwm failed, code:%d, %s", errcode, errstr)
	}
	return nil
}

func (x *XStreamConn) GetRecord() (Message, error) {
	var lcr unsafe.Pointer = C.malloc(C.size_t(1))
	var lcrType C.uchar
	var flag C.ulong
	var fetchlwm = (*C.uchar)(C.calloc(C.OCI_LCR_MAX_POSITION_LEN, 8))
	defer C.free(unsafe.Pointer(fetchlwm))
	var fetchlwm_len C.ushort
	status := C.OCIXStreamOutLCRReceive(x.ocip.svcp, x.ocip.errp, &lcr, &lcrType,
		&flag, fetchlwm, &fetchlwm_len, C.OCI_DEFAULT)
	if status == C.OCI_STILL_EXECUTING {
		return getLcrRecords(x.ocip, lcr, x.csid, x.ncsid)
	}
	if status == C.OCI_ERROR {
		errstr, errcode := getError(x.ocip.errp)
		return nil, fmt.Errorf("OCIXStreamOutLCRReceive failed, code:%d, %s", errcode, errstr)
	}
	s := pos2SCN(x.ocip, fetchlwm, fetchlwm_len)
	C.OCILCRFree(x.ocip.svcp, x.ocip.errp, lcr, C.OCI_DEFAULT)
	C.free(lcr)
	return &HeartBeat{SCN: s}, nil
}

func tostring(p *C.uchar, l C.ushort) string {
	return string(*(*[]byte)((unsafe.Pointer)(&reflect.SliceHeader{Data: uintptr(unsafe.Pointer(p)), Len: int(uint16(l)), Cap: int(uint16(l))})))
}

func toStringEnc(p *C.uchar, l C.ushort, codepage int) (string, error) {
	b := *(*[]byte)((unsafe.Pointer)(&reflect.SliceHeader{
		Data: uintptr(unsafe.Pointer(p)),
		Len:  int(uint16(l)),
		Cap:  int(uint16(l)),
	}))
	dec := decoders[codepage]
	if dec == nil {
		log.Panicf("code page %d not defined", codepage)
	} else {
		return dec(b)
	}
	return "", nil
}

func tobytes(p *C.uchar, l C.ushort) []byte {
	ret := make([]byte, uint16(l))
	copy(ret, *(*[]byte)((unsafe.Pointer)(&reflect.SliceHeader{
		Data: uintptr(unsafe.Pointer(p)),
		Len:  int(uint16(l)),
		Cap:  int(uint16(l)),
	})))
	return ret
}

func getLcrRecords(ocip *C.struct_oci, lcr unsafe.Pointer, csid, ncsid int) (Message, error) {
	var cmd_type, owner, oname, txid *C.oratext
	var cmd_type_len, ownerl, onamel, txidl C.ub2
	var src_db_name **C.oratext
	var src_db_name_l *C.ub2
	var ret C.sword
	var ltag, lpos *C.ub1
	var ltagl, lposl, oldCount, newCount C.ub2
	var dummy C.oraub8
	var t C.OCIDate
	ret = C.OCILCRHeaderGet(ocip.svcp, ocip.errp,
		src_db_name, src_db_name_l, &cmd_type, &cmd_type_len,
		&owner, &ownerl, &oname, &onamel, &ltag, &ltagl, &txid, &txidl,
		&t, &oldCount, &newCount,
		&lpos, &lposl, &dummy, lcr,
		C.OCI_DEFAULT)
	if ret != C.OCI_SUCCESS {
		C.ocierror(ocip, C.CString("OCILCRHeaderGet failed"))
	} else {
		cmd := tostring(cmd_type, cmd_type_len)
		s := pos2SCN(ocip, lpos, lposl)
		switch cmd {
		case "COMMIT":
			m := Commit{SCN: s}
			return &m, nil
		case "DELETE":
			stringEnc, err := toStringEnc(oname, onamel, csid)
			if err != nil {
				return nil, err
			}
			m := Delete{SCN: s, Table: stringEnc, Owner: tostring(owner, ownerl)}
			m.OldColumn, m.OldRow, err = getLcrRowData(ocip, lcr, valueTypeOld, csid, ncsid, m.Owner+"."+m.Table)
			return &m, err
		case "INSERT":
			stringEnc, err := toStringEnc(oname, onamel, csid)
			if err != nil {
				return nil, err
			}
			m := Insert{SCN: s, Table: stringEnc, Owner: tostring(owner, ownerl)}
			m.NewColumn, m.NewRow, err = getLcrRowData(ocip, lcr, valueTypeNew, csid, ncsid, m.Owner+"."+m.Table)
			return &m, err
		case "UPDATE":
			stringEnc, err := toStringEnc(oname, onamel, csid)
			if err != nil {
				return nil, err
			}
			m := Update{SCN: s, Table: stringEnc, Owner: tostring(owner, ownerl)}
			m.OldColumn, m.OldRow, err = getLcrRowData(ocip, lcr, valueTypeOld, csid, ncsid, m.Owner+"."+m.Table)
			if err != nil {
				return nil, err
			}
			m.NewColumn, m.NewRow, err = getLcrRowData(ocip, lcr, valueTypeNew, csid, ncsid, m.Owner+"."+m.Table)
			return &m, err
		}
	}
	return nil, nil
}

type valueType C.ub2

var valueTypeOld valueType = C.OCI_LCR_ROW_COLVAL_OLD
var valueTypeNew valueType = C.OCI_LCR_ROW_COLVAL_NEW

var ownerm = make(map[string]columnInfo)

type columnInfo struct {
	count     int
	names     **C.oratext
	namesLens *C.ub2
	inDP      *C.OCIInd
	cSetFP    *C.ub1
	flags     *C.oraub8
}

func getLcrRowData(ocip *C.struct_oci, lcrp unsafe.Pointer, valueType valueType, csid, ncsid int, owner string) ([]string, []interface{}, error) {
	const colCount = 256
	var col_names **C.oratext
	var col_names_lens *C.ub2
	var column_indp *C.OCIInd
	var column_csetfp *C.ub1
	var column_flags *C.oraub8
	if ci, ok := ownerm[owner]; !ok {
		col_names = (**C.oratext)(unsafe.Pointer(cgo.NewIntN(colCount)))
		col_names_lens = (*C.ub2)(cgo.NewUInt16N(colCount))
		column_indp = (*C.OCIInd)(C.calloc(C.size_t(colCount), C.size_t(unsafe.Sizeof(C.OCIInd(0)))))
		column_csetfp = (*C.ub1)(cgo.NewUInt8N(colCount))
		column_flags = (*C.oraub8)(cgo.NewUInt64N(colCount))
		ownerm[owner] = columnInfo{
			count:     256,
			names:     col_names,
			namesLens: col_names_lens,
			inDP:      column_indp,
			cSetFP:    column_csetfp,
			flags:     column_flags,
		}
	} else {
		col_names = ci.names
		col_names_lens = ci.namesLens
		column_indp = ci.inDP
		column_csetfp = ci.cSetFP
		column_flags = ci.flags
	}

	var result C.sword
	var num_cols C.ub2
	var col_dtype [colCount]C.ub2
	var column_valuesp [colCount]*C.void
	var column_alensp [colCount]C.ub2
	var column_csid [colCount]C.ub2
	//defer func() {
	//	C.free(unsafe.Pointer(col_names))
	//	C.free(unsafe.Pointer(col_names_lens))
	//	C.free(unsafe.Pointer(column_indp))
	//	C.free(unsafe.Pointer(column_csetfp))
	//	C.free(unsafe.Pointer(column_flags))
	//}()
	result = C.OCILCRRowColumnInfoGet(
		ocip.svcp, ocip.errp,
		C.ushort(valueType), &num_cols,
		col_names, col_names_lens,
		(*C.ub2)(unsafe.Pointer(&col_dtype)),
		(*unsafe.Pointer)((unsafe.Pointer)(&column_valuesp)),
		column_indp,
		(*C.ub2)(unsafe.Pointer(&column_alensp)),
		column_csetfp,
		column_flags,
		(*C.ub2)(unsafe.Pointer(&column_csid)),
		lcrp,
		C.ushort(colCount),
		C.OCI_DEFAULT,
	)
	if result != C.OCI_SUCCESS {
		errstr, errcode := getError(ocip.errp)
		return nil, nil, fmt.Errorf("OCIXStreamOutLCRReceive failed, code:%d, %s", errcode, errstr)
	} else {
		columnNames := make([]string, 0)
		columnValues := make([]interface{}, 0)
		for i := 0; i < int(uint16(num_cols)); i++ {
			colName := tostring((*C.uchar)((*[colCount]unsafe.Pointer)(unsafe.Pointer(col_names))[i]),
				C.ushort((*[colCount]uint16)(unsafe.Pointer(col_names_lens))[i]))
			columnNames = append(columnNames, colName)
			colValuep := column_valuesp[i]
			colDtype := col_dtype[i]
			csid_l := int(column_csid[i])
			if csid_l == 0 {
				csid_l = csid
			}
			colValue := value2interface(ocip.errp, colValuep, column_alensp[i], csid_l, colDtype)
			columnValues = append(columnValues, colValue)
		}
		return columnNames, columnValues, nil
	}
}

func value2interface(errp *C.OCIError, valuep *C.void, valuelen C.ub2, csid int, dtype C.ub2) interface{} {
	if valuelen == 0 {
		return nil
	}
	switch dtype {
	//todo support more types
	case C.SQLT_CHR, C.SQLT_AFC:
		val, err := toStringEnc((*C.uchar)(unsafe.Pointer(valuep)), valuelen, int(csid))
		if err != nil {
			panic(err)
		}
		return val
	case C.SQLT_VNU:
		v := (*C.OCINumber)(unsafe.Pointer(valuep))
		if v == nil {
			return nil
		}
		return ociNumberToInt(errp, v)
	case C.SQLT_ODT:
		v := (*C.OCIDate)(unsafe.Pointer(valuep))
		yy := int16(v.OCIDateYYYY)
		mm := uint8(v.OCIDateMM)
		dd := uint8(v.OCIDateDD)
		dt := v.OCIDateTime
		hh := uint8(dt.OCITimeHH)
		min := uint8(dt.OCITimeMI)
		ss := uint8(dt.OCITimeSS)
		return time.Date(int(yy), time.Month(mm), int(dd), int(hh), int(min), int(ss), 0, time.Local)
	}
	return nil
}

func getError(oci_err *C.OCIError) (string, int32) {
	errCode := C.sb4(0)
	text := [4096]C.text{}
	C.OCIErrorGet(unsafe.Pointer(oci_err), C.uint(1),
		(*C.text)(unsafe.Pointer(nil)), &errCode, (*C.uchar)(unsafe.Pointer(&text)), 4096, C.OCI_HTYPE_ERROR)
	return cgo.GoString((*cgo.Char)(unsafe.Pointer(&text))), int32(errCode)
}

func getErrorEnc(oci_err *C.OCIError, csid int) (string, int32, error) {
	errCode := C.sb4(0)
	text := [4096]C.text{}
	C.OCIErrorGet(unsafe.Pointer(oci_err), C.uint(1),
		(*C.text)(unsafe.Pointer(nil)), &errCode, (*C.uchar)(unsafe.Pointer(&text)), 4096, C.OCI_HTYPE_ERROR)
	var l int
	for i, c := range text {
		if int(C.ushort(c)) == 0 {
			l = i
			break
		}
	}
	val, err := toStringEnc((*C.uchar)(unsafe.Pointer(&text)), C.ushort(l), csid)
	return val, int32(errCode), err
}

func pos2SCN(ocip *C.struct_oci, pos *C.ub1, pos_len C.ub2) scn.SCN {
	if pos_len == 0 {
		return 0
	}
	var s C.struct_OCINumber
	var commit_scn C.struct_OCINumber
	var result C.sword
	result = C.OCILCRSCNsFromPosition(ocip.svcp, ocip.errp, pos, pos_len, &s, &commit_scn, C.OCI_DEFAULT)
	if result != C.OCI_SUCCESS {
		// todo
		C.ocierror(ocip, C.CString("OCILCRHeaderGet failed"))
		return 0
	} else {
		return scn.SCN(ociNumberToInt(ocip.errp, &s))
	}
}

func scn2pos(ocip *C.struct_oci, s scn.SCN) (*C.ub1, C.ub2) {
	var number *C.OCINumber = ociNumberFromInt(ocip.errp, int64(s))
	pos := (*C.ub1)(C.calloc(33, 1))
	var posl C.ub2
	result := C.OCILCRSCNToPosition2(ocip.svcp, ocip.errp, pos, &posl, number, C.OCI_LCRID_V2, C.OCI_DEFAULT)
	if result != C.OCI_SUCCESS {
		// todo
		C.ocierror(ocip, C.CString("OCILCRHeaderGet failed"))
		return nil, 0
	} else {
		return pos, posl
	}
}
