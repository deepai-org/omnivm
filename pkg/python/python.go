// Package python embeds CPython via cgo.
//
// Build requires: python3-dev headers and libpython3.
// The cgo directives use pkg-config to find the correct flags.
package python

/*
#cgo pkg-config: python-3.14-embed
#include <Python.h>
#include <endian.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <fcntl.h>
#include <pthread.h>
#ifdef __GLIBC__
#include <execinfo.h>
#endif

// Bridge callback pointer — set via omnivm_py_set_bridge_callback().
typedef char* (*omni_call_fn)(const char* runtime, const char* code);
typedef void (*omni_free_fn)(char* ptr);
static omni_call_fn g_bridge_call = NULL;
static omni_free_fn g_bridge_free = NULL;

// Buffer bridge function pointers — set via omnivm_py_set_buf_callbacks().
// get: fill an omni_buffer_t from a named shared buffer. Returns 0 on success.
// set: register a buffer under a name. Returns 0 on success.
// release: schedule deferred release (safe from GC threads).
typedef struct {
    void*   data;
    int64_t len;
    int32_t dtype;
    int8_t  owned;
    int8_t  read_only;
} py_omni_buffer_t;

typedef int (*omni_buf_get_fn)(const char* name, py_omni_buffer_t* out);
typedef int (*omni_buf_set_fn)(const char* name, py_omni_buffer_t buf);
typedef void (*omni_buf_release_fn)(const char* name);

static omni_buf_get_fn g_buf_get = NULL;
static omni_buf_set_fn g_buf_set = NULL;
static omni_buf_release_fn g_buf_release = NULL;

static void omnivm_py_set_buf_callbacks(omni_buf_get_fn get_fn,
                                         omni_buf_set_fn set_fn,
                                         omni_buf_release_fn release_fn) {
    g_buf_get = get_fn;
    g_buf_set = set_fn;
    g_buf_release = release_fn;
}

static const char* omnivm_py_runtime_error_code =
"import builtins as __omnivm_builtins\n"
"import json as __omnivm_json\n"
"import re as __omnivm_re\n"
"class RuntimeError(__omnivm_builtins.RuntimeError):\n"
"    def __init__(self, message, runtime=None, boundary_path=None):\n"
"        super().__init__(message)\n"
"        parsed = _parse_runtime_error_text(str(message), runtime=runtime or None, boundary_path=boundary_path or None)\n"
"        self.runtime = parsed['runtime']\n"
"        self.type = parsed['type']\n"
"        self.message = parsed['message']\n"
"        self.traceback = parsed['traceback']\n"
"        self.cause_chain = parsed['cause_chain']\n"
"        self.boundary_path = parsed['boundary_path']\n"
"        self.original_error_handle = parsed['original_error_handle']\n"
"        self.details = parsed['details']\n"
"    def to_dict(self):\n"
"        return {'runtime': self.runtime, 'type': self.type, 'message': self.message, 'traceback': self.traceback, 'cause_chain': list(self.cause_chain), 'boundary_path': self.boundary_path, 'original_error_handle': self.original_error_handle, 'details': self.details}\n"
"def _is_error_type_candidate(candidate):\n"
"    return bool(__omnivm_re.match(r'^[A-Za-z_][A-Za-z0-9_.$:]*$', candidate or ''))\n"
"def _is_runtime_error_metadata_line(line):\n"
"    lower = (line or '').strip().lower()\n"
"    return lower.startswith('caused by:') or lower.startswith('details:') or lower.startswith('original_error_handle:') or lower.startswith('original error handle:') or lower.startswith('original-error-handle:')\n"
"def _parse_runtime_error_details(text):\n"
"    for line in str(text).splitlines():\n"
"        stripped = line.strip()\n"
"        if not stripped.startswith('Details: '):\n"
"            continue\n"
"        try:\n"
"            value = __omnivm_json.loads(stripped[len('Details: '):])\n"
"        except Exception:\n"
"            return None\n"
"        if isinstance(value, dict):\n"
"            return value\n"
"        if isinstance(value, list):\n"
"            return {'errors': value}\n"
"        return {'value': value}\n"
"    return None\n"
"def _parse_runtime_error_text(text, runtime=None, boundary_path=None):\n"
"    source_runtime = runtime\n"
"    body = text[4:] if text.startswith('ERR:') else text\n"
"    boundary_parts = []\n"
"    for marker, label in (('execute manifest: ', 'execute manifest'), ('load manifest module: ', 'load manifest module'), ('manifest module call: ', 'manifest module call')):\n"
"        if body.startswith(marker):\n"
"            boundary_parts.append(label)\n"
"            body = body[len(marker):]\n"
"            break\n"
"    op_match = __omnivm_re.match(r'(?P<op>[A-Za-z_][A-Za-z0-9_]*) \\[(?P<runtime>[A-Za-z0-9_-]+)\\]: (?P<body>.*)', body, __omnivm_re.S)\n"
"    if op_match:\n"
"        boundary_parts.append(f\"{op_match.group('op')}[{op_match.group('runtime')}]\")\n"
"        source_runtime = op_match.group('runtime')\n"
"        body = op_match.group('body')\n"
"    runtime_ref_assign_match = __omnivm_re.match(r'runtime ref assign \\[(?P<runtime>[A-Za-z0-9_-]+)\\]: (?P<body>.*)', body, __omnivm_re.S)\n"
"    if runtime_ref_assign_match:\n"
"        source_runtime = runtime_ref_assign_match.group('runtime')\n"
"        body = runtime_ref_assign_match.group('body')\n"
"    changed = True\n"
"    while changed:\n"
"        changed = False\n"
"        for prefix, canonical in (('javascript: ', 'javascript'), ('python: ', 'python'), ('ruby: ', 'ruby'), ('jvm: ', 'java'), ('java: ', 'java'), ('go: ', 'go')):\n"
"            if body.startswith(prefix):\n"
"                source_runtime = canonical\n"
"                body = body[len(prefix):]\n"
"                changed = True\n"
"                break\n"
"    first_line, _, rest = body.partition('\\n')\n"
"    err_type = ''\n"
"    detail = first_line\n"
"    handle_match = __omnivm_re.search(r'(?im)^\\s*(?:Original[- ]error[- ]handle|original_error_handle):\\s*(?P<handle>\\S+)\\s*$', body)\n"
"    original_error_handle = handle_match.group('handle') if handle_match else None\n"
"    parse_line = first_line\n"
"    traceback = rest\n"
"    if first_line.startswith('Traceback '):\n"
"        traceback = body\n"
"        for line in reversed([line.strip() for line in body.splitlines() if line.strip()]):\n"
"            if _is_runtime_error_metadata_line(line):\n"
"                continue\n"
"            if ': ' not in line:\n"
"                continue\n"
"            candidate, _tail = line.split(': ', 1)\n"
"            if _is_error_type_candidate(candidate):\n"
"                parse_line = line\n"
"                break\n"
"    if ': ' in parse_line:\n"
"        candidate, tail = parse_line.split(': ', 1)\n"
"        if _is_error_type_candidate(candidate):\n"
"            if source_runtime == 'python' and '.' in candidate:\n"
"                candidate = candidate.rsplit('.', 1)[-1]\n"
"            err_type = candidate\n"
"            detail = tail\n"
"    cause_chain = []\n"
"    for line in rest.splitlines():\n"
"        stripped = line.strip()\n"
"        if not stripped.startswith('Caused by: '):\n"
"            continue\n"
"        cause_text = stripped[len('Caused by: '):]\n"
"        cause_type = ''\n"
"        cause_message = cause_text\n"
"        if ': ' in cause_text:\n"
"            candidate, tail = cause_text.split(': ', 1)\n"
"            if _is_error_type_candidate(candidate):\n"
"                cause_type = candidate\n"
"                cause_message = tail\n"
"        cause_chain.append({'type': cause_type, 'message': cause_message})\n"
"    return {'runtime': source_runtime, 'type': err_type, 'message': detail, 'traceback': traceback, 'cause_chain': cause_chain, 'boundary_path': ' > '.join(boundary_parts) or (f'call[{source_runtime}]' if source_runtime and source_runtime != runtime else boundary_path), 'original_error_handle': original_error_handle, 'details': _parse_runtime_error_details(body)}\n"
;

static void omnivm_py_raise_runtime_error(const char* runtime, const char* message, const char* boundary_path) {
    PyObject* mod = PyImport_ImportModule("omnivm");
    PyObject* cls = mod ? PyObject_GetAttrString(mod, "RuntimeError") : NULL;
    if (!cls) {
        Py_XDECREF(mod);
        PyErr_SetString(PyExc_RuntimeError, message ? message : "runtime error");
        return;
    }
    PyObject* args = PyTuple_New(1);
    PyObject* msg = PyUnicode_FromString(message ? message : "");
    if (!args || !msg) {
        Py_XDECREF(args);
        Py_XDECREF(msg);
        Py_DECREF(cls);
        Py_XDECREF(mod);
        PyErr_SetString(PyExc_RuntimeError, message ? message : "runtime error");
        return;
    }
    PyTuple_SET_ITEM(args, 0, msg);
    PyObject* kwargs = PyDict_New();
    if (kwargs) {
        if (runtime && runtime[0]) {
            PyObject* rt = PyUnicode_FromString(runtime);
            if (rt) {
                PyDict_SetItemString(kwargs, "runtime", rt);
                Py_DECREF(rt);
            }
        }
        if (boundary_path && boundary_path[0]) {
            PyObject* path = PyUnicode_FromString(boundary_path);
            if (path) {
                PyDict_SetItemString(kwargs, "boundary_path", path);
                Py_DECREF(path);
            }
        }
    }
    PyObject* exc = PyObject_Call(cls, args, kwargs);
    Py_DECREF(args);
    Py_XDECREF(kwargs);
    if (exc) {
        PyErr_SetObject(cls, exc);
        Py_DECREF(exc);
    } else {
        PyErr_SetString(PyExc_RuntimeError, message ? message : "runtime error");
    }
    Py_DECREF(cls);
    Py_XDECREF(mod);
}

typedef struct ArrowSchema {
    const char* format;
    const char* name;
    const char* metadata;
    int64_t flags;
    int64_t n_children;
    struct ArrowSchema** children;
    struct ArrowSchema* dictionary;
    void (*release)(struct ArrowSchema*);
    void* private_data;
} ArrowSchema;

typedef struct ArrowArray {
    int64_t length;
    int64_t null_count;
    int64_t offset;
    int64_t n_buffers;
    int64_t n_children;
    const void** buffers;
    struct ArrowArray** children;
    struct ArrowArray* dictionary;
    void (*release)(struct ArrowArray*);
    void* private_data;
} ArrowArray;

typedef struct ArrowArrayStream {
    int (*get_schema)(struct ArrowArrayStream*, ArrowSchema* out);
    int (*get_next)(struct ArrowArrayStream*, ArrowArray* out);
    const char* (*get_last_error)(struct ArrowArrayStream*);
    void (*release)(struct ArrowArrayStream*);
    void* private_data;
} ArrowArrayStream;

typedef struct {
    int32_t device_type;
    int32_t device_id;
} DLDevice;

typedef struct {
    uint8_t code;
    uint8_t bits;
    uint16_t lanes;
} DLDataType;

typedef struct {
    void* data;
    DLDevice device;
    int32_t ndim;
    DLDataType dtype;
    int64_t* shape;
    int64_t* strides;
    uint64_t byte_offset;
} DLTensor;

typedef struct DLManagedTensor {
    DLTensor dl_tensor;
    void* manager_ctx;
    void (*deleter)(struct DLManagedTensor* self);
} DLManagedTensor;

#define kDLCPU 1

static int omnivm_py_endian_is_host_compatible(const char* endian) {
	if (!endian || endian[0] == '\0') return 1;
	if (strcmp(endian, "=") == 0 || strcmp(endian, "|") == 0) return 1;
#if __BYTE_ORDER == __LITTLE_ENDIAN
	return strcmp(endian, "<") == 0;
#elif __BYTE_ORDER == __BIG_ENDIAN
	return strcmp(endian, ">") == 0;
#else
	return 0;
#endif
}

typedef struct {
    PyObject_HEAD
    char* name;
    void* data;
    Py_ssize_t len;
    int read_only;
} py_omnivm_buffer_view_t;

static void py_omnivm_buffer_view_dealloc(py_omnivm_buffer_view_t* self) {
    if (g_buf_release && self->name) {
        g_buf_release(self->name);
    }
    if (self->name) free(self->name);
    Py_TYPE(self)->tp_free((PyObject*)self);
}

static int py_omnivm_buffer_view_getbuffer(PyObject* exporter, Py_buffer* view, int flags) {
    py_omnivm_buffer_view_t* self = (py_omnivm_buffer_view_t*)exporter;
    return PyBuffer_FillInfo(view, exporter, self->data, self->len, self->read_only, flags);
}

static void py_omnivm_buffer_view_releasebuffer(PyObject* exporter, Py_buffer* view) {
    (void)exporter;
    (void)view;
}

static PyBufferProcs py_omnivm_buffer_view_as_buffer = {
    .bf_getbuffer = py_omnivm_buffer_view_getbuffer,
    .bf_releasebuffer = py_omnivm_buffer_view_releasebuffer,
};

static PyTypeObject py_omnivm_buffer_view_type = {
    PyVarObject_HEAD_INIT(NULL, 0)
    .tp_name = "omnivm._BufferView",
    .tp_basicsize = sizeof(py_omnivm_buffer_view_t),
    .tp_dealloc = (destructor)py_omnivm_buffer_view_dealloc,
    .tp_flags = Py_TPFLAGS_DEFAULT,
    .tp_as_buffer = &py_omnivm_buffer_view_as_buffer,
};

static PyObject* py_omnivm_memoryview_from_buffer(const char* name, py_omni_buffer_t* buf) {
	py_omnivm_buffer_view_t* owner =
		PyObject_New(py_omnivm_buffer_view_t, &py_omnivm_buffer_view_type);
    if (!owner) {
        if (g_buf_release) g_buf_release(name);
        return NULL;
    }
    owner->data = buf->data;
    owner->len = (Py_ssize_t)buf->len;
    owner->read_only = buf->read_only != 0;
    owner->name = strdup(name);
	if (!owner->name) {
		PyObject_Del(owner);
		if (g_buf_release) g_buf_release(name);
		return PyErr_NoMemory();
	}
	return (PyObject*)owner;
}

typedef struct {
	Py_buffer view;
	PyObject* owner;
	PyObject* schema_capsule;
	PyObject* array_capsule;
	ArrowSchema* arrow_schema;
	ArrowArray* arrow_array;
	PyObject* stream_capsule;
	ArrowArrayStream* stream;
	ArrowSchema* stream_schema;
	ArrowArray* stream_array;
	PyObject* dlpack_capsule;
	DLManagedTensor* dlpack_managed;
	Py_buffer aux_view;
	void* data;
	void* validity;
	Py_ssize_t len;
	Py_ssize_t validity_len;
	Py_ssize_t validity_bit_offset;
	Py_ssize_t itemsize;
	Py_ssize_t null_count;
	int readonly;
	char* format;
	int ndim;
	Py_ssize_t shape[8];
	Py_ssize_t strides[8];
	Py_ssize_t offset;
	int has_buffer;
	int has_aux_buffer;
} py_omnivm_exported_buffer_t;

static int omnivm_py_arrow_c_format(const char* format, char* out, size_t out_len, Py_ssize_t* itemsize) {
	if (!format || !out || out_len < 2 || !itemsize) return 0;
	if (strlen(format) != 1) return 0;
	switch (format[0]) {
	case 'c':
		out[0] = 'b';
		*itemsize = 1;
		break;
	case 'C':
		out[0] = 'B';
		*itemsize = 1;
		break;
	case 's':
		out[0] = 'h';
		*itemsize = 2;
		break;
	case 'S':
		out[0] = 'H';
		*itemsize = 2;
		break;
	case 'i':
		out[0] = 'i';
		*itemsize = 4;
		break;
	case 'I':
		out[0] = 'I';
		*itemsize = 4;
		break;
	case 'l':
		out[0] = 'q';
		*itemsize = 8;
		break;
	case 'L':
		out[0] = 'Q';
		*itemsize = 8;
		break;
	case 'f':
		out[0] = 'f';
		*itemsize = 4;
		break;
	case 'g':
		out[0] = 'd';
		*itemsize = 8;
		break;
	default:
		return 0;
	}
	out[1] = '\0';
	return 1;
}

static int omnivm_py_dlpack_format(DLDataType dtype, char* out, size_t out_len, Py_ssize_t* itemsize) {
	if (!out || out_len < 2 || !itemsize || dtype.lanes != 1 || dtype.bits == 0 || dtype.bits % 8 != 0) return 0;
	*itemsize = (Py_ssize_t)(dtype.bits / 8);
	switch (dtype.code) {
	case 0:
		if (dtype.bits == 8) out[0] = 'b';
		else if (dtype.bits == 16) out[0] = 'h';
		else if (dtype.bits == 32) out[0] = 'i';
		else if (dtype.bits == 64) out[0] = 'q';
		else return 0;
		break;
	case 1:
		if (dtype.bits == 8) out[0] = 'B';
		else if (dtype.bits == 16) out[0] = 'H';
		else if (dtype.bits == 32) out[0] = 'I';
		else if (dtype.bits == 64) out[0] = 'Q';
		else return 0;
		break;
	case 2:
		if (dtype.bits == 32) out[0] = 'f';
		else if (dtype.bits == 64) out[0] = 'd';
		else return 0;
		break;
	default:
		return 0;
	}
	out[1] = '\0';
	return 1;
}

static int omnivm_py_device_allows_cpu_export(PyObject* obj, int require_method) {
	PyObject* method = PyObject_GetAttrString(obj, "__dlpack_device__");
	if (!method) {
		PyErr_Clear();
		return require_method ? 0 : 1;
	}
	if (!PyCallable_Check(method)) {
		Py_DECREF(method);
		return require_method ? 0 : 1;
	}
	PyObject* result = PyObject_CallFunctionObjArgs(method, NULL);
	Py_DECREF(method);
	if (!result) {
		PyErr_Clear();
		return require_method ? 0 : 1;
	}
	if (!PySequence_Check(result) || PySequence_Size(result) < 1) {
		Py_DECREF(result);
		return require_method ? 0 : 1;
	}
	PyObject* device_obj = PySequence_GetItem(result, 0);
	Py_DECREF(result);
	if (!device_obj) {
		PyErr_Clear();
		return require_method ? 0 : 1;
	}
	long device_type = PyLong_AsLong(device_obj);
	Py_DECREF(device_obj);
	if (PyErr_Occurred()) {
		PyErr_Clear();
		return require_method ? 0 : 1;
	}
	return device_type == kDLCPU;
}

static int omnivm_py_dlpack_device_allows_cpu_export(PyObject* obj) {
	return omnivm_py_device_allows_cpu_export(obj, 0);
}

static int omnivm_py_dataframe_buffer_allows_cpu_export(PyObject* obj) {
	return omnivm_py_device_allows_cpu_export(obj, 1);
}

static int omnivm_py_array_interface_format(const char* typestr, char* out, size_t out_len, Py_ssize_t* itemsize) {
	if (!typestr || !out || out_len < 2 || !itemsize) return 0;
	const char* cursor = typestr;
	char endian[2] = {0};
	if (*cursor == '<' || *cursor == '>' || *cursor == '=' || *cursor == '|') {
		endian[0] = *cursor;
		cursor++;
	}
	if (*cursor == '\0') return 0;
	char kind = *cursor;
	cursor++;
	char* end = NULL;
	long size = strtol(cursor, &end, 10);
	if (end == cursor || *end != '\0' || size <= 0) return 0;
	if (size > 1 && !omnivm_py_endian_is_host_compatible(endian[0] ? endian : NULL)) return 0;
	switch (kind) {
	case 'i':
		if (size == 1) out[0] = 'b';
		else if (size == 2) out[0] = 'h';
		else if (size == 4) out[0] = 'i';
		else if (size == 8) out[0] = 'q';
		else return 0;
		break;
	case 'u':
		if (size == 1) out[0] = 'B';
		else if (size == 2) out[0] = 'H';
		else if (size == 4) out[0] = 'I';
		else if (size == 8) out[0] = 'Q';
		else return 0;
		break;
	case 'f':
		if (size == 4) out[0] = 'f';
		else if (size == 8) out[0] = 'd';
		else return 0;
		break;
	default:
		return 0;
	}
	out[1] = '\0';
	*itemsize = (Py_ssize_t)size;
	return 1;
}

static int omnivm_py_tuple_dim_at(PyObject* seq, Py_ssize_t i, Py_ssize_t* out) {
	PyObject* item = PySequence_GetItem(seq, i);
	if (!item) return 0;
	Py_ssize_t value = PyLong_AsSsize_t(item);
	Py_DECREF(item);
	if (value < 0 || PyErr_Occurred()) {
		PyErr_Clear();
		return 0;
	}
	*out = value;
	return 1;
}

static int omnivm_py_tuple_stride_at(PyObject* seq, Py_ssize_t i, Py_ssize_t* out) {
	PyObject* item = PySequence_GetItem(seq, i);
	if (!item) return 0;
	Py_ssize_t value = PyLong_AsSsize_t(item);
	Py_DECREF(item);
	if (value == 0 || PyErr_Occurred()) {
		PyErr_Clear();
		return 0;
	}
	*out = value;
	return 1;
}

static int omnivm_py_strided_bounds(Py_ssize_t ndim, Py_ssize_t* shape, Py_ssize_t* strides, Py_ssize_t itemsize, Py_ssize_t* min_offset, Py_ssize_t* span) {
	if (!shape || !strides || !min_offset || !span || ndim <= 0 || itemsize <= 0) return 0;
	Py_ssize_t min = 0;
	Py_ssize_t max = itemsize;
	for (Py_ssize_t i = 0; i < ndim; i++) {
		Py_ssize_t dim = shape[i];
		Py_ssize_t stride = strides[i];
		if (dim < 0 || stride == 0) return 0;
		if (dim == 0) {
			*min_offset = 0;
			*span = 0;
			return 1;
		}
		Py_ssize_t steps = dim - 1;
		if (steps <= 0) continue;
		if (stride > 0) {
			if (stride > (PY_SSIZE_T_MAX - max) / steps) return 0;
			max += stride * steps;
		} else {
			if (stride < PY_SSIZE_T_MIN / steps) return 0;
			Py_ssize_t delta = stride * steps;
			if (min < PY_SSIZE_T_MIN - delta) return 0;
			min += delta;
		}
	}
	if (max < min || max - min <= 0) return 0;
	*min_offset = min;
	*span = max - min;
	return 1;
}

static py_omnivm_exported_buffer_t* omnivm_py_export_array_interface(PyObject* obj) {
	PyObject* iface = PyObject_GetAttrString(obj, "__array_interface__");
	if (!iface) {
		PyErr_Clear();
		return NULL;
	}
	if (!PyDict_Check(iface)) {
		Py_DECREF(iface);
		return NULL;
	}
	PyObject* data_obj = PyDict_GetItemString(iface, "data");
	PyObject* shape_obj = PyDict_GetItemString(iface, "shape");
	PyObject* typestr_obj = PyDict_GetItemString(iface, "typestr");
	if (!data_obj || !shape_obj || !typestr_obj || !PyUnicode_Check(typestr_obj) || !PySequence_Check(shape_obj)) {
		Py_DECREF(iface);
		return NULL;
	}
	void* data = NULL;
	int readonly = 1;
	Py_buffer data_view;
	memset(&data_view, 0, sizeof(data_view));
	int has_data_view = 0;
	if (PyTuple_Check(data_obj) && PyTuple_Size(data_obj) >= 1) {
		PyObject* ptr_obj = PyTuple_GetItem(data_obj, 0);
		unsigned long long ptr = PyLong_AsUnsignedLongLong(ptr_obj);
		if (PyErr_Occurred()) {
			PyErr_Clear();
			Py_DECREF(iface);
			return NULL;
		}
		data = (void*)(uintptr_t)ptr;
		if (PyTuple_Size(data_obj) >= 2) {
			readonly = PyObject_IsTrue(PyTuple_GetItem(data_obj, 1)) ? 1 : 0;
		}
	} else if (PyLong_Check(data_obj)) {
		unsigned long long ptr = PyLong_AsUnsignedLongLong(data_obj);
		if (PyErr_Occurred()) {
			PyErr_Clear();
			Py_DECREF(iface);
			return NULL;
		}
		data = (void*)(uintptr_t)ptr;
	} else {
		if (PyObject_GetBuffer(data_obj, &data_view, PyBUF_SIMPLE) != 0) {
			PyErr_Clear();
			Py_DECREF(iface);
			return NULL;
		}
		has_data_view = 1;
		data = data_view.buf;
		readonly = data_view.readonly ? 1 : 0;
	}

	Py_ssize_t ndim = PySequence_Size(shape_obj);
	if (ndim <= 0 || ndim > 8) {
		if (has_data_view) PyBuffer_Release(&data_view);
		Py_DECREF(iface);
		return NULL;
	}
	const char* typestr = PyUnicode_AsUTF8(typestr_obj);
	char format[2] = {0};
	Py_ssize_t itemsize = 0;
	if (!omnivm_py_array_interface_format(typestr, format, sizeof(format), &itemsize)) {
		if (has_data_view) PyBuffer_Release(&data_view);
		Py_DECREF(iface);
		return NULL;
	}

	Py_ssize_t shape[8] = {0};
	Py_ssize_t strides[8] = {0};
	Py_ssize_t elements = 1;
	for (Py_ssize_t i = 0; i < ndim; i++) {
		if (!omnivm_py_tuple_dim_at(shape_obj, i, &shape[i])) {
			if (has_data_view) PyBuffer_Release(&data_view);
			Py_DECREF(iface);
			return NULL;
		}
		if (shape[i] > 0 && elements > PY_SSIZE_T_MAX / shape[i]) {
			if (has_data_view) PyBuffer_Release(&data_view);
			Py_DECREF(iface);
			return NULL;
		}
		elements *= shape[i];
	}
	strides[ndim - 1] = itemsize;
	for (Py_ssize_t i = ndim - 1; i > 0; i--) {
		if (shape[i] > 0 && strides[i] > PY_SSIZE_T_MAX / shape[i]) {
			if (has_data_view) PyBuffer_Release(&data_view);
			Py_DECREF(iface);
			return NULL;
		}
		strides[i - 1] = strides[i] * shape[i];
	}
	PyObject* strides_obj = PyDict_GetItemString(iface, "strides");
	if (strides_obj && strides_obj != Py_None) {
		if (!PySequence_Check(strides_obj) || PySequence_Size(strides_obj) != ndim) {
			if (has_data_view) PyBuffer_Release(&data_view);
			Py_DECREF(iface);
			return NULL;
		}
		for (Py_ssize_t i = 0; i < ndim; i++) {
			Py_ssize_t stride = 0;
			if (!omnivm_py_tuple_stride_at(strides_obj, i, &stride)) {
				if (has_data_view) PyBuffer_Release(&data_view);
				Py_DECREF(iface);
				return NULL;
			}
			strides[i] = stride;
		}
	}
	if (elements > PY_SSIZE_T_MAX / itemsize || (elements > 0 && data == NULL)) {
		if (has_data_view) PyBuffer_Release(&data_view);
		Py_DECREF(iface);
		return NULL;
	}
	Py_ssize_t min_offset = 0;
	Py_ssize_t byte_span = 0;
	if (!omnivm_py_strided_bounds(ndim, shape, strides, itemsize, &min_offset, &byte_span)) {
		if (has_data_view) PyBuffer_Release(&data_view);
		Py_DECREF(iface);
		return NULL;
	}
	if (has_data_view) {
		if (min_offset < 0 || byte_span < 0 || min_offset > data_view.len ||
		    byte_span > data_view.len - min_offset) {
			PyBuffer_Release(&data_view);
			Py_DECREF(iface);
			return NULL;
		}
	}

	py_omnivm_exported_buffer_t* exported =
		(py_omnivm_exported_buffer_t*)calloc(1, sizeof(py_omnivm_exported_buffer_t));
	if (!exported) {
		if (has_data_view) PyBuffer_Release(&data_view);
		Py_DECREF(iface);
		return NULL;
	}
	Py_INCREF(obj);
	exported->owner = obj;
	if (has_data_view) {
		exported->aux_view = data_view;
		exported->has_aux_buffer = 1;
	}
	exported->data = data ? (void*)((char*)data + min_offset) : NULL;
	exported->len = byte_span;
	exported->itemsize = itemsize;
	exported->readonly = readonly;
	exported->format = strdup(format);
	exported->ndim = (int)ndim;
	exported->offset = -min_offset;
	for (Py_ssize_t i = 0; i < ndim; i++) {
		exported->shape[i] = shape[i];
		exported->strides[i] = strides[i];
	}
	Py_DECREF(iface);
	if (!exported->format) {
		if (exported->has_aux_buffer) PyBuffer_Release(&exported->aux_view);
		Py_DECREF(exported->owner);
		free(exported);
		return NULL;
	}
	return exported;
}

static py_omnivm_exported_buffer_t* omnivm_py_export_arrow_c_array(PyObject* obj) {
	PyObject* method = PyObject_GetAttrString(obj, "__arrow_c_array__");
	if (!method) {
		PyErr_Clear();
		return NULL;
	}
	if (!PyCallable_Check(method)) {
		Py_DECREF(method);
		return NULL;
	}

	PyObject* result = PyObject_CallFunctionObjArgs(method, Py_None, NULL);
	if (!result && PyErr_ExceptionMatches(PyExc_TypeError)) {
		PyErr_Clear();
		result = PyObject_CallFunctionObjArgs(method, NULL);
	}
	Py_DECREF(method);
	if (!result) {
		PyErr_Clear();
		return NULL;
	}
	if (!PyTuple_Check(result) || PyTuple_Size(result) != 2) {
		Py_DECREF(result);
		return NULL;
	}

	PyObject* schema_capsule = PyTuple_GetItem(result, 0);
	PyObject* array_capsule = PyTuple_GetItem(result, 1);
	if (!PyCapsule_IsValid(schema_capsule, "arrow_schema") || !PyCapsule_IsValid(array_capsule, "arrow_array")) {
		Py_DECREF(result);
		return NULL;
	}
	ArrowSchema* schema = (ArrowSchema*)PyCapsule_GetPointer(schema_capsule, "arrow_schema");
	ArrowArray* array = (ArrowArray*)PyCapsule_GetPointer(array_capsule, "arrow_array");
	if (!schema || !array) {
		PyErr_Clear();
		Py_DECREF(result);
		return NULL;
	}
	if (!schema->release || !array->release || schema->n_children != 0 || schema->dictionary != NULL ||
	    array->length < 0 || array->offset < 0 || array->null_count < 0 || array->null_count > array->length ||
	    array->n_children != 0 || array->dictionary != NULL || array->n_buffers < 2 || !array->buffers) {
		Py_DECREF(result);
		return NULL;
	}

	char format[2] = {0};
	Py_ssize_t itemsize = 0;
	if (!omnivm_py_arrow_c_format(schema->format, format, sizeof(format), &itemsize)) {
		Py_DECREF(result);
		return NULL;
	}
	if (array->length > PY_SSIZE_T_MAX || array->offset > PY_SSIZE_T_MAX) {
		Py_DECREF(result);
		return NULL;
	}
	Py_ssize_t length = (Py_ssize_t)array->length;
	Py_ssize_t offset = (Py_ssize_t)array->offset;
	if (length > 0 && array->buffers[1] == NULL) {
		Py_DECREF(result);
		return NULL;
	}
	if (array->null_count > 0 && array->buffers[0] == NULL) {
		Py_DECREF(result);
		return NULL;
	}
	if (offset > 0 && length > PY_SSIZE_T_MAX - offset) {
		Py_DECREF(result);
		return NULL;
	}
	Py_ssize_t backing_elements = offset + length;
	if (backing_elements > 0 && backing_elements > PY_SSIZE_T_MAX / itemsize) {
		Py_DECREF(result);
		return NULL;
	}
	Py_ssize_t validity_len = array->null_count > 0 ? (backing_elements + 7) / 8 : 0;
	if (PyCapsule_SetName(schema_capsule, "used_arrow_schema") != 0) {
		PyErr_Clear();
		Py_DECREF(result);
		return NULL;
	}
	if (PyCapsule_SetName(array_capsule, "used_arrow_array") != 0) {
		PyErr_Clear();
		PyCapsule_SetName(schema_capsule, "arrow_schema");
		Py_DECREF(result);
		return NULL;
	}

	py_omnivm_exported_buffer_t* exported =
		(py_omnivm_exported_buffer_t*)calloc(1, sizeof(py_omnivm_exported_buffer_t));
	if (!exported) {
		if (array->release) array->release(array);
		if (schema->release) schema->release(schema);
		Py_DECREF(result);
		return NULL;
	}
	Py_INCREF(schema_capsule);
	Py_INCREF(array_capsule);
	exported->schema_capsule = schema_capsule;
	exported->array_capsule = array_capsule;
	exported->arrow_schema = schema;
	exported->arrow_array = array;
	Py_INCREF(obj);
	exported->owner = obj;
	exported->data = (void*)array->buffers[1];
	exported->validity = array->null_count > 0 ? (void*)array->buffers[0] : NULL;
	exported->len = backing_elements * itemsize;
	exported->validity_len = validity_len;
	exported->validity_bit_offset = offset;
	exported->itemsize = itemsize;
	exported->null_count = (Py_ssize_t)array->null_count;
	exported->readonly = 1;
	exported->format = strdup(format);
	exported->ndim = 1;
	exported->shape[0] = length;
	exported->strides[0] = itemsize;
	exported->offset = offset * itemsize;
	Py_DECREF(result);
	if (!exported->format) {
		if (exported->arrow_array && exported->arrow_array->release) exported->arrow_array->release(exported->arrow_array);
		if (exported->arrow_schema && exported->arrow_schema->release) exported->arrow_schema->release(exported->arrow_schema);
		if (exported->owner) Py_DECREF(exported->owner);
		Py_DECREF(exported->schema_capsule);
		Py_DECREF(exported->array_capsule);
		free(exported);
		return NULL;
	}
	return exported;
}

static py_omnivm_exported_buffer_t* omnivm_py_export_arrow_c_stream(PyObject* obj) {
	PyObject* method = PyObject_GetAttrString(obj, "__arrow_c_stream__");
	if (!method) {
		PyErr_Clear();
		return NULL;
	}
	if (!PyCallable_Check(method)) {
		Py_DECREF(method);
		return NULL;
	}

	PyObject* capsule = PyObject_CallFunctionObjArgs(method, Py_None, NULL);
	if (!capsule && PyErr_ExceptionMatches(PyExc_TypeError)) {
		PyErr_Clear();
		capsule = PyObject_CallFunctionObjArgs(method, NULL);
	}
	Py_DECREF(method);
	if (!capsule) {
		PyErr_Clear();
		return NULL;
	}
	if (!PyCapsule_IsValid(capsule, "arrow_array_stream")) {
		Py_DECREF(capsule);
		return NULL;
	}

	ArrowArrayStream* stream = (ArrowArrayStream*)PyCapsule_GetPointer(capsule, "arrow_array_stream");
	if (!stream || !stream->get_schema || !stream->get_next || !stream->release) {
		PyErr_Clear();
		Py_DECREF(capsule);
		return NULL;
	}
	if (PyCapsule_SetName(capsule, "used_arrow_array_stream") != 0) {
		PyErr_Clear();
		Py_DECREF(capsule);
		return NULL;
	}
	ArrowSchema* schema = (ArrowSchema*)calloc(1, sizeof(ArrowSchema));
	ArrowArray* array = (ArrowArray*)calloc(1, sizeof(ArrowArray));
	if (!schema || !array) {
		if (schema) free(schema);
		if (array) free(array);
		stream->release(stream);
		Py_DECREF(capsule);
		return NULL;
	}
	if (stream->get_schema(stream, schema) != 0 || !schema->release ||
	    stream->get_next(stream, array) != 0 || !array->release) {
		if (array->release) array->release(array);
		if (schema->release) schema->release(schema);
		stream->release(stream);
		free(array);
		free(schema);
		Py_DECREF(capsule);
		return NULL;
	}

	ArrowArray eos;
	memset(&eos, 0, sizeof(eos));
	if (stream->get_next(stream, &eos) != 0 || eos.release != NULL) {
		if (eos.release) eos.release(&eos);
		array->release(array);
		schema->release(schema);
		stream->release(stream);
		free(array);
		free(schema);
		Py_DECREF(capsule);
		return NULL;
	}

	if (schema->n_children != 0 || schema->dictionary != NULL || array->length < 0 || array->offset < 0 ||
	    array->null_count < 0 || array->null_count > array->length ||
	    array->n_children != 0 || array->dictionary != NULL || array->n_buffers < 2 || !array->buffers) {
		array->release(array);
		schema->release(schema);
		stream->release(stream);
		free(array);
		free(schema);
		Py_DECREF(capsule);
		return NULL;
	}

	char format[2] = {0};
	Py_ssize_t itemsize = 0;
	if (!omnivm_py_arrow_c_format(schema->format, format, sizeof(format), &itemsize)) {
		array->release(array);
		schema->release(schema);
		stream->release(stream);
		free(array);
		free(schema);
		Py_DECREF(capsule);
		return NULL;
	}
	if (array->length > PY_SSIZE_T_MAX || array->offset > PY_SSIZE_T_MAX) {
		array->release(array);
		schema->release(schema);
		stream->release(stream);
		free(array);
		free(schema);
		Py_DECREF(capsule);
		return NULL;
	}
	Py_ssize_t length = (Py_ssize_t)array->length;
	Py_ssize_t offset = (Py_ssize_t)array->offset;
	if (length > 0 && array->buffers[1] == NULL) {
		array->release(array);
		schema->release(schema);
		stream->release(stream);
		free(array);
		free(schema);
		Py_DECREF(capsule);
		return NULL;
	}
	if (array->null_count > 0 && array->buffers[0] == NULL) {
		array->release(array);
		schema->release(schema);
		stream->release(stream);
		free(array);
		free(schema);
		Py_DECREF(capsule);
		return NULL;
	}
	if (offset > 0 && length > PY_SSIZE_T_MAX - offset) {
		array->release(array);
		schema->release(schema);
		stream->release(stream);
		free(array);
		free(schema);
		Py_DECREF(capsule);
		return NULL;
	}
	Py_ssize_t backing_elements = offset + length;
	if (backing_elements > 0 && backing_elements > PY_SSIZE_T_MAX / itemsize) {
		array->release(array);
		schema->release(schema);
		stream->release(stream);
		free(array);
		free(schema);
		Py_DECREF(capsule);
		return NULL;
	}
	Py_ssize_t validity_len = array->null_count > 0 ? (backing_elements + 7) / 8 : 0;

	py_omnivm_exported_buffer_t* exported =
		(py_omnivm_exported_buffer_t*)calloc(1, sizeof(py_omnivm_exported_buffer_t));
	if (!exported) {
		array->release(array);
		schema->release(schema);
		stream->release(stream);
		free(array);
		free(schema);
		Py_DECREF(capsule);
		return NULL;
	}
	exported->stream_capsule = capsule;
	exported->stream = stream;
	exported->stream_schema = schema;
	exported->stream_array = array;
	Py_INCREF(obj);
	exported->owner = obj;
	exported->data = (void*)array->buffers[1];
	exported->validity = array->null_count > 0 ? (void*)array->buffers[0] : NULL;
	exported->len = backing_elements * itemsize;
	exported->validity_len = validity_len;
	exported->validity_bit_offset = offset;
	exported->itemsize = itemsize;
	exported->null_count = (Py_ssize_t)array->null_count;
	exported->readonly = 1;
	exported->format = strdup(format);
	exported->ndim = 1;
	exported->shape[0] = length;
	exported->strides[0] = itemsize;
	exported->offset = offset * itemsize;
	if (!exported->format) {
		if (exported->stream_array && exported->stream_array->release) exported->stream_array->release(exported->stream_array);
		if (exported->stream_schema && exported->stream_schema->release) exported->stream_schema->release(exported->stream_schema);
		if (exported->stream && exported->stream->release) exported->stream->release(exported->stream);
		if (exported->owner) Py_DECREF(exported->owner);
		Py_DECREF(exported->stream_capsule);
		free(exported->stream_array);
		free(exported->stream_schema);
		free(exported);
		return NULL;
	}
	return exported;
}

static py_omnivm_exported_buffer_t* omnivm_py_export_dlpack(PyObject* obj) {
	if (!omnivm_py_dlpack_device_allows_cpu_export(obj)) {
		return NULL;
	}
	PyObject* method = PyObject_GetAttrString(obj, "__dlpack__");
	if (!method) {
		PyErr_Clear();
		return NULL;
	}
	if (!PyCallable_Check(method)) {
		Py_DECREF(method);
		return NULL;
	}

	PyObject* capsule = PyObject_CallFunctionObjArgs(method, NULL);
	if (!capsule && PyErr_ExceptionMatches(PyExc_TypeError)) {
		PyErr_Clear();
		capsule = PyObject_CallFunctionObjArgs(method, Py_None, NULL);
	}
	Py_DECREF(method);
	if (!capsule) {
		PyErr_Clear();
		return NULL;
	}
	if (!PyCapsule_IsValid(capsule, "dltensor")) {
		Py_DECREF(capsule);
		return NULL;
	}

	DLManagedTensor* managed = (DLManagedTensor*)PyCapsule_GetPointer(capsule, "dltensor");
	if (!managed) {
		PyErr_Clear();
		Py_DECREF(capsule);
		return NULL;
	}
	DLTensor* tensor = &managed->dl_tensor;
	if (!tensor->data || tensor->device.device_type != kDLCPU || tensor->ndim <= 0 ||
	    tensor->ndim > 8 || !tensor->shape || tensor->byte_offset > (uint64_t)PY_SSIZE_T_MAX) {
		Py_DECREF(capsule);
		return NULL;
	}

	char format[2] = {0};
	Py_ssize_t itemsize = 0;
	if (!omnivm_py_dlpack_format(tensor->dtype, format, sizeof(format), &itemsize)) {
		Py_DECREF(capsule);
		return NULL;
	}

	Py_ssize_t shape[8] = {0};
	Py_ssize_t byte_strides[8] = {0};
	for (int32_t i = 0; i < tensor->ndim; i++) {
		if (tensor->shape[i] < 0 || tensor->shape[i] > PY_SSIZE_T_MAX) {
			Py_DECREF(capsule);
			return NULL;
		}
		shape[i] = (Py_ssize_t)tensor->shape[i];
	}
	if (tensor->strides) {
		for (int32_t i = 0; i < tensor->ndim; i++) {
			if (tensor->strides[i] == 0 ||
			    tensor->strides[i] > PY_SSIZE_T_MAX / itemsize ||
			    tensor->strides[i] < PY_SSIZE_T_MIN / itemsize) {
				Py_DECREF(capsule);
				return NULL;
			}
			byte_strides[i] = (Py_ssize_t)(tensor->strides[i] * itemsize);
		}
	} else {
		byte_strides[tensor->ndim - 1] = itemsize;
		for (int32_t i = tensor->ndim - 1; i > 0; i--) {
			if (shape[i] > 0 && byte_strides[i] > PY_SSIZE_T_MAX / shape[i]) {
				Py_DECREF(capsule);
				return NULL;
			}
			byte_strides[i - 1] = byte_strides[i] * shape[i];
		}
	}

	Py_ssize_t min_offset = 0;
	Py_ssize_t byte_span = 0;
	if (!omnivm_py_strided_bounds(tensor->ndim, shape, byte_strides, itemsize, &min_offset, &byte_span)) {
		Py_DECREF(capsule);
		return NULL;
	}
	if (PyCapsule_SetName(capsule, "used_dltensor") != 0) {
		PyErr_Clear();
		Py_DECREF(capsule);
		return NULL;
	}

	py_omnivm_exported_buffer_t* exported =
		(py_omnivm_exported_buffer_t*)calloc(1, sizeof(py_omnivm_exported_buffer_t));
	if (!exported) {
		if (managed->deleter) managed->deleter(managed);
		Py_DECREF(capsule);
		return NULL;
	}
	exported->dlpack_capsule = capsule;
	exported->dlpack_managed = managed;
	exported->data = (void*)((char*)tensor->data + (Py_ssize_t)tensor->byte_offset + min_offset);
	exported->len = byte_span;
	exported->itemsize = itemsize;
	exported->readonly = 0;
	exported->format = strdup(format);
	exported->ndim = (int)tensor->ndim;
	exported->offset = -min_offset;
	for (int32_t i = 0; i < tensor->ndim; i++) {
		exported->shape[i] = shape[i];
		exported->strides[i] = byte_strides[i];
	}
	if (!exported->format) {
		if (exported->dlpack_managed && exported->dlpack_managed->deleter) {
			exported->dlpack_managed->deleter(exported->dlpack_managed);
		}
		Py_DECREF(exported->dlpack_capsule);
		free(exported);
		return NULL;
	}
	return exported;
}

static PyObject* omnivm_py_call_noarg_method(PyObject* obj, const char* name) {
	PyObject* method = PyObject_GetAttrString(obj, name);
	if (!method) {
		PyErr_Clear();
		return NULL;
	}
	if (!PyCallable_Check(method)) {
		Py_DECREF(method);
		return NULL;
	}
	PyObject* result = PyObject_CallFunctionObjArgs(method, NULL);
	Py_DECREF(method);
	if (!result) {
		PyErr_Clear();
		return NULL;
	}
	return result;
}

static int omnivm_py_noarg_method_int(PyObject* obj, const char* name, Py_ssize_t* out) {
	PyObject* result = omnivm_py_call_noarg_method(obj, name);
	if (!result) return 0;
	Py_ssize_t value = PyLong_AsSsize_t(result);
	Py_DECREF(result);
	if (value < 0 || PyErr_Occurred()) {
		PyErr_Clear();
		return 0;
	}
	*out = value;
	return 1;
}

static int omnivm_py_attr_int(PyObject* obj, const char* name, Py_ssize_t* out) {
	PyObject* value_obj = PyObject_GetAttrString(obj, name);
	if (!value_obj) {
		PyErr_Clear();
		return 0;
	}
	if (value_obj == Py_None) {
		Py_DECREF(value_obj);
		return 0;
	}
	Py_ssize_t value = PyLong_AsSsize_t(value_obj);
	Py_DECREF(value_obj);
	if (value < 0 || PyErr_Occurred()) {
		PyErr_Clear();
		return 0;
	}
	*out = value;
	return 1;
}

static PyObject* omnivm_py_dataframe_exchange_object(PyObject* obj) {
	PyObject* method = PyObject_GetAttrString(obj, "__dataframe__");
	if (!method) {
		PyErr_Clear();
		return NULL;
	}
	if (!PyCallable_Check(method)) {
		Py_DECREF(method);
		return NULL;
	}

	PyObject* args = PyTuple_New(0);
	PyObject* kwargs = Py_BuildValue("{s:O}", "allow_copy", Py_False);
	PyObject* result = NULL;
	if (args && kwargs) {
		result = PyObject_Call(method, args, kwargs);
	}
	Py_XDECREF(args);
	Py_XDECREF(kwargs);
	if (!result && PyErr_ExceptionMatches(PyExc_TypeError)) {
		PyErr_Clear();
		result = PyObject_CallFunctionObjArgs(method, NULL);
	}
	Py_DECREF(method);
	if (!result) {
		PyErr_Clear();
		return NULL;
	}
	return result;
}

static int omnivm_py_dataframe_dtype_format(PyObject* dtype_obj, char* out, size_t out_len, Py_ssize_t* itemsize) {
	if (!dtype_obj || !PySequence_Check(dtype_obj) || PySequence_Size(dtype_obj) < 3) return 0;
	PyObject* kind_obj = PySequence_GetItem(dtype_obj, 0);
	PyObject* bits_obj = PySequence_GetItem(dtype_obj, 1);
	PyObject* format_obj = PySequence_GetItem(dtype_obj, 2);
	PyObject* endian_obj = PySequence_Size(dtype_obj) >= 4 ? PySequence_GetItem(dtype_obj, 3) : NULL;
	if (!kind_obj || !bits_obj || !format_obj || !PyUnicode_Check(format_obj)) {
		Py_XDECREF(kind_obj);
		Py_XDECREF(bits_obj);
		Py_XDECREF(format_obj);
		Py_XDECREF(endian_obj);
		return 0;
	}
	long kind = PyLong_AsLong(kind_obj);
	long bits = PyLong_AsLong(bits_obj);
	const char* format = PyUnicode_AsUTF8(format_obj);
	const char* endian = NULL;
	if (endian_obj && endian_obj != Py_None) {
		if (!PyUnicode_Check(endian_obj)) {
			Py_DECREF(kind_obj);
			Py_DECREF(bits_obj);
			Py_DECREF(format_obj);
			Py_DECREF(endian_obj);
			return 0;
		}
		endian = PyUnicode_AsUTF8(endian_obj);
	}
	if (PyErr_Occurred() || !format || bits <= 0 || bits % 8 != 0 ||
	    (kind != 0 && kind != 1 && kind != 2) ||
	    !omnivm_py_endian_is_host_compatible(endian)) {
		PyErr_Clear();
		Py_DECREF(kind_obj);
		Py_DECREF(bits_obj);
		Py_DECREF(format_obj);
		Py_XDECREF(endian_obj);
		return 0;
	}
	Py_ssize_t parsed_itemsize = 0;
	if (!omnivm_py_arrow_c_format(format, out, out_len, &parsed_itemsize) ||
	    parsed_itemsize != (Py_ssize_t)(bits / 8)) {
		Py_DECREF(kind_obj);
		Py_DECREF(bits_obj);
		Py_DECREF(format_obj);
		Py_XDECREF(endian_obj);
		return 0;
	}
	*itemsize = parsed_itemsize;
	Py_DECREF(kind_obj);
	Py_DECREF(bits_obj);
	Py_DECREF(format_obj);
	Py_XDECREF(endian_obj);
	return 1;
}

static Py_ssize_t omnivm_py_count_validity_nulls(const unsigned char* validity, Py_ssize_t bit_offset, Py_ssize_t length) {
	if (!validity || bit_offset < 0 || length < 0) return -1;
	Py_ssize_t nulls = 0;
	for (Py_ssize_t i = 0; i < length; i++) {
		Py_ssize_t bit = bit_offset + i;
		unsigned char byte = validity[bit / 8];
		unsigned char mask = (unsigned char)(1u << (bit % 8));
		if ((byte & mask) == 0) nulls++;
	}
	return nulls;
}

static py_omnivm_exported_buffer_t* omnivm_py_export_dataframe_interchange(PyObject* obj) {
	PyObject* df = omnivm_py_dataframe_exchange_object(obj);
	if (!df) return NULL;

	Py_ssize_t count = 0;
	if (!omnivm_py_noarg_method_int(df, "num_columns", &count) || count != 1) {
		Py_DECREF(df);
		return NULL;
	}
	if (omnivm_py_noarg_method_int(df, "num_chunks", &count) && count != 1) {
		Py_DECREF(df);
		return NULL;
	}

	PyObject* column = PyObject_CallMethod(df, "get_column", "n", (Py_ssize_t)0);
	if (!column) {
		PyErr_Clear();
		Py_DECREF(df);
		return NULL;
	}
	if (omnivm_py_noarg_method_int(column, "num_chunks", &count) && count != 1) {
		Py_DECREF(column);
		Py_DECREF(df);
		return NULL;
	}

	Py_ssize_t size = 0;
	if (!omnivm_py_noarg_method_int(column, "size", &size)) {
		Py_DECREF(column);
		Py_DECREF(df);
		return NULL;
	}
	Py_ssize_t column_offset = 0;
	omnivm_py_attr_int(column, "offset", &column_offset);

	PyObject* buffers = omnivm_py_call_noarg_method(column, "get_buffers");
	if (!buffers || !PyMapping_Check(buffers)) {
		Py_XDECREF(buffers);
		Py_DECREF(column);
		Py_DECREF(df);
		return NULL;
	}
	PyObject* data_entry = PyMapping_GetItemString(buffers, "data");
	PyObject* validity_entry = PyMapping_GetItemString(buffers, "validity");
	if (!validity_entry) PyErr_Clear();
	PyObject* offsets_entry = PyMapping_GetItemString(buffers, "offsets");
	if (!offsets_entry) PyErr_Clear();
	if (!data_entry || data_entry == Py_None ||
	    (offsets_entry && offsets_entry != Py_None) ||
	    !PySequence_Check(data_entry) || PySequence_Size(data_entry) < 2) {
		Py_XDECREF(data_entry);
		Py_XDECREF(validity_entry);
		Py_XDECREF(offsets_entry);
		Py_DECREF(buffers);
		Py_DECREF(column);
		Py_DECREF(df);
		return NULL;
	}
	Py_XDECREF(offsets_entry);

	PyObject* buffer_obj = PySequence_GetItem(data_entry, 0);
	PyObject* dtype_obj = PySequence_GetItem(data_entry, 1);
	Py_DECREF(data_entry);
	if (!buffer_obj || !dtype_obj) {
		Py_XDECREF(buffer_obj);
		Py_XDECREF(dtype_obj);
		Py_XDECREF(validity_entry);
		Py_DECREF(buffers);
		Py_DECREF(column);
		Py_DECREF(df);
		return NULL;
	}
	if (!omnivm_py_dataframe_buffer_allows_cpu_export(buffer_obj)) {
		Py_DECREF(dtype_obj);
		Py_DECREF(buffer_obj);
		Py_XDECREF(validity_entry);
		Py_DECREF(buffers);
		Py_DECREF(column);
		Py_DECREF(df);
		return NULL;
	}

	char format[2] = {0};
	Py_ssize_t itemsize = 0;
	if (!omnivm_py_dataframe_dtype_format(dtype_obj, format, sizeof(format), &itemsize)) {
		Py_DECREF(dtype_obj);
		Py_DECREF(buffer_obj);
		Py_XDECREF(validity_entry);
		Py_DECREF(buffers);
		Py_DECREF(column);
		Py_DECREF(df);
		return NULL;
	}
	Py_DECREF(dtype_obj);

	PyObject* validity_buffer_obj = NULL;
	unsigned long long validity_ptr_value = 0;
	Py_ssize_t validity_len = 0;
	Py_ssize_t null_count = 0;
	if (validity_entry && validity_entry != Py_None) {
		if (!PySequence_Check(validity_entry) || PySequence_Size(validity_entry) < 2) {
			Py_DECREF(buffer_obj);
			Py_DECREF(validity_entry);
			Py_DECREF(buffers);
			Py_DECREF(column);
			Py_DECREF(df);
			return NULL;
		}
		validity_buffer_obj = PySequence_GetItem(validity_entry, 0);
		if (!validity_buffer_obj || !omnivm_py_dataframe_buffer_allows_cpu_export(validity_buffer_obj)) {
			Py_XDECREF(validity_buffer_obj);
			Py_DECREF(buffer_obj);
			Py_DECREF(validity_entry);
			Py_DECREF(buffers);
			Py_DECREF(column);
			Py_DECREF(df);
			return NULL;
		}
		Py_ssize_t validity_bufsize = 0;
		if (!omnivm_py_attr_int(validity_buffer_obj, "bufsize", &validity_bufsize)) {
			Py_DECREF(validity_buffer_obj);
			Py_DECREF(buffer_obj);
			Py_DECREF(validity_entry);
			Py_DECREF(buffers);
			Py_DECREF(column);
			Py_DECREF(df);
			return NULL;
		}
		PyObject* validity_ptr_obj = PyObject_GetAttrString(validity_buffer_obj, "ptr");
		if (!validity_ptr_obj) {
			PyErr_Clear();
			Py_DECREF(validity_buffer_obj);
			Py_DECREF(buffer_obj);
			Py_DECREF(validity_entry);
			Py_DECREF(buffers);
			Py_DECREF(column);
			Py_DECREF(df);
			return NULL;
		}
		validity_ptr_value = PyLong_AsUnsignedLongLong(validity_ptr_obj);
		Py_DECREF(validity_ptr_obj);
		if (PyErr_Occurred() || validity_bufsize < 0 || column_offset < 0 || size < 0 ||
		    column_offset > PY_SSIZE_T_MAX - size) {
			PyErr_Clear();
			Py_DECREF(validity_buffer_obj);
			Py_DECREF(buffer_obj);
			Py_DECREF(validity_entry);
			Py_DECREF(buffers);
			Py_DECREF(column);
			Py_DECREF(df);
			return NULL;
		}
		validity_len = (column_offset + size + 7) / 8;
		if ((validity_len > 0 && validity_ptr_value == 0) || validity_len > validity_bufsize) {
			Py_DECREF(validity_buffer_obj);
			Py_DECREF(buffer_obj);
			Py_DECREF(validity_entry);
			Py_DECREF(buffers);
			Py_DECREF(column);
			Py_DECREF(df);
			return NULL;
		}
		null_count = omnivm_py_count_validity_nulls(
			(const unsigned char*)(uintptr_t)validity_ptr_value, column_offset, size);
		if (null_count < 0) {
			Py_DECREF(validity_buffer_obj);
			Py_DECREF(buffer_obj);
			Py_DECREF(validity_entry);
			Py_DECREF(buffers);
			Py_DECREF(column);
			Py_DECREF(df);
			return NULL;
		}
	}
	Py_XDECREF(validity_entry);

	Py_ssize_t bufsize = 0;
	if (!omnivm_py_attr_int(buffer_obj, "bufsize", &bufsize)) {
		Py_XDECREF(validity_buffer_obj);
		Py_DECREF(buffer_obj);
		Py_DECREF(buffers);
		Py_DECREF(column);
		Py_DECREF(df);
		return NULL;
	}
	PyObject* ptr_obj = PyObject_GetAttrString(buffer_obj, "ptr");
	if (!ptr_obj) {
		PyErr_Clear();
		Py_XDECREF(validity_buffer_obj);
		Py_DECREF(buffer_obj);
		Py_DECREF(buffers);
		Py_DECREF(column);
		Py_DECREF(df);
		return NULL;
	}
	unsigned long long ptr_value = PyLong_AsUnsignedLongLong(ptr_obj);
	Py_DECREF(ptr_obj);
	if (PyErr_Occurred() || (bufsize > 0 && ptr_value == 0) ||
	    column_offset < 0 || size < 0 ||
	    column_offset > PY_SSIZE_T_MAX / itemsize ||
	    size > PY_SSIZE_T_MAX / itemsize) {
		PyErr_Clear();
		Py_XDECREF(validity_buffer_obj);
		Py_DECREF(buffer_obj);
		Py_DECREF(buffers);
		Py_DECREF(column);
		Py_DECREF(df);
		return NULL;
	}
	Py_ssize_t byte_offset = column_offset * itemsize;
	if (byte_offset > bufsize || size * itemsize > bufsize - byte_offset) {
		Py_XDECREF(validity_buffer_obj);
		Py_DECREF(buffer_obj);
		Py_DECREF(buffers);
		Py_DECREF(column);
		Py_DECREF(df);
		return NULL;
	}

	py_omnivm_exported_buffer_t* exported =
		(py_omnivm_exported_buffer_t*)calloc(1, sizeof(py_omnivm_exported_buffer_t));
	if (!exported) {
		Py_XDECREF(validity_buffer_obj);
		Py_DECREF(buffer_obj);
		Py_DECREF(buffers);
		Py_DECREF(column);
		Py_DECREF(df);
		return NULL;
	}
	PyObject* validity_owner = validity_buffer_obj ? validity_buffer_obj : Py_None;
	exported->owner = PyTuple_Pack(5, obj, df, column, buffer_obj, validity_owner);
	exported->data = (void*)(uintptr_t)ptr_value;
	exported->validity = validity_len > 0 ? (void*)(uintptr_t)validity_ptr_value : NULL;
	exported->len = bufsize;
	exported->validity_len = validity_len;
	exported->validity_bit_offset = column_offset;
	exported->itemsize = itemsize;
	exported->null_count = null_count;
	exported->readonly = 1;
	exported->format = strdup(format);
	exported->ndim = 1;
	exported->shape[0] = size;
	exported->strides[0] = itemsize;
	exported->offset = byte_offset;
	Py_XDECREF(validity_buffer_obj);
	Py_DECREF(buffer_obj);
	Py_DECREF(buffers);
	Py_DECREF(column);
	Py_DECREF(df);
	if (!exported->owner || !exported->format) {
		Py_XDECREF(exported->owner);
		if (exported->format) free(exported->format);
		free(exported);
		return NULL;
	}
	return exported;
}

static int omnivm_py_buffer_has_supported_strides(Py_buffer* view);

static py_omnivm_exported_buffer_t* omnivm_py_export_array_method(PyObject* obj) {
	PyObject* method = PyObject_GetAttrString(obj, "__array__");
	if (!method) {
		PyErr_Clear();
		return NULL;
	}
	if (!PyCallable_Check(method)) {
		Py_DECREF(method);
		return NULL;
	}

	PyObject* array_obj = PyObject_CallFunctionObjArgs(method, NULL);
	Py_DECREF(method);
	if (!array_obj) {
		PyErr_Clear();
		return NULL;
	}

	py_omnivm_exported_buffer_t* exported =
		(py_omnivm_exported_buffer_t*)calloc(1, sizeof(py_omnivm_exported_buffer_t));
	if (exported && PyObject_GetBuffer(array_obj, &exported->view, PyBUF_FULL_RO) == 0) {
		exported->has_buffer = 1;
		Py_DECREF(array_obj);
		if (!omnivm_py_buffer_has_supported_strides(&exported->view)) {
			PyBuffer_Release(&exported->view);
			free(exported);
			return NULL;
		}
		return exported;
	}
	PyErr_Clear();
	if (exported) {
		free(exported);
	}

	exported = omnivm_py_export_arrow_c_array(array_obj);
	if (!exported) {
		exported = omnivm_py_export_arrow_c_stream(array_obj);
	}
	if (!exported) {
		exported = omnivm_py_export_dlpack(array_obj);
	}
	if (!exported) {
		exported = omnivm_py_export_dataframe_interchange(array_obj);
	}
	if (!exported) {
		exported = omnivm_py_export_array_interface(array_obj);
	}
	Py_DECREF(array_obj);
	return exported;
}

static int omnivm_py_buffer_has_supported_strides(Py_buffer* view) {
	if (!view) return 0;
	if (view->ndim > 8) return 0;
	if (view->suboffsets && view->ndim > 0) {
		for (int i = 0; i < view->ndim; i++) {
			if (view->suboffsets[i] >= 0) return 0;
		}
	}
	if (!view->shape || !view->strides || view->ndim <= 0) {
		return PyBuffer_IsContiguous(view, 'C');
	}
	for (int i = 0; i < view->ndim; i++) {
		if (view->shape[i] < 0 || view->strides[i] == 0) return 0;
	}
	return 1;
}

static int omnivm_py_exported_buffer_bounds(py_omnivm_exported_buffer_t* exported, Py_ssize_t* min_offset, Py_ssize_t* span) {
	if (!exported) return 0;
	if (!exported->has_buffer) {
		if (min_offset) *min_offset = -exported->offset;
		if (span) *span = exported->len;
		return 1;
	}
	Py_buffer* view = &exported->view;
	if (!view->shape || !view->strides || view->ndim <= 0) {
		if (min_offset) *min_offset = 0;
		if (span) *span = view->len;
		return view->len >= 0;
	}
	return omnivm_py_strided_bounds(view->ndim, view->shape, view->strides, view->itemsize, min_offset, span);
}

static Py_ssize_t omnivm_py_exported_buffer_span(py_omnivm_exported_buffer_t* exported) {
	Py_ssize_t min_offset = 0;
	Py_ssize_t span = 0;
	if (!omnivm_py_exported_buffer_bounds(exported, &min_offset, &span)) return -1;
	return span;
}

static py_omnivm_exported_buffer_t* omnivm_py_export_buffer(const char* expr) {
	PyGILState_STATE gstate = PyGILState_Ensure();
	PyObject* main_module = PyImport_AddModule("__main__");
	if (!main_module) {
		PyGILState_Release(gstate);
		return NULL;
	}
	PyObject* globals = PyModule_GetDict(main_module);
	PyObject* obj = PyRun_String(expr, Py_eval_input, globals, globals);
	if (!obj) {
		PyErr_Clear();
		PyGILState_Release(gstate);
		return NULL;
	}

	py_omnivm_exported_buffer_t* exported =
		(py_omnivm_exported_buffer_t*)calloc(1, sizeof(py_omnivm_exported_buffer_t));
	if (!exported) {
		Py_DECREF(obj);
		PyGILState_Release(gstate);
		return NULL;
	}
	if (PyObject_GetBuffer(obj, &exported->view, PyBUF_FULL_RO) != 0) {
		PyErr_Clear();
		free(exported);
		exported = omnivm_py_export_arrow_c_array(obj);
		if (!exported) {
			exported = omnivm_py_export_arrow_c_stream(obj);
		}
		if (!exported) {
			exported = omnivm_py_export_dlpack(obj);
		}
		if (!exported) {
			exported = omnivm_py_export_dataframe_interchange(obj);
		}
		if (!exported) {
			exported = omnivm_py_export_array_interface(obj);
		}
		if (!exported) {
			exported = omnivm_py_export_array_method(obj);
		}
		Py_DECREF(obj);
		PyGILState_Release(gstate);
		return exported;
	}
	exported->has_buffer = 1;
	Py_DECREF(obj);
	if (!omnivm_py_buffer_has_supported_strides(&exported->view)) {
		PyBuffer_Release(&exported->view);
		free(exported);
		PyGILState_Release(gstate);
		return NULL;
	}
	PyGILState_Release(gstate);
	return exported;
}

static void omnivm_py_release_exported_buffer(py_omnivm_exported_buffer_t* exported) {
	if (!exported) return;
	PyGILState_STATE gstate = PyGILState_Ensure();
	if (exported->has_buffer) {
		PyBuffer_Release(&exported->view);
	}
	if (exported->has_aux_buffer) {
		PyBuffer_Release(&exported->aux_view);
		exported->has_aux_buffer = 0;
	}
	if (exported->arrow_array) {
		if (exported->arrow_array->release) {
			exported->arrow_array->release(exported->arrow_array);
		}
		exported->arrow_array = NULL;
	}
	if (exported->arrow_schema) {
		if (exported->arrow_schema->release) {
			exported->arrow_schema->release(exported->arrow_schema);
		}
		exported->arrow_schema = NULL;
	}
	if (exported->schema_capsule) {
		Py_DECREF(exported->schema_capsule);
	}
	if (exported->array_capsule) {
		Py_DECREF(exported->array_capsule);
	}
	if (exported->stream_array) {
		if (exported->stream_array->release) {
			exported->stream_array->release(exported->stream_array);
		}
		free(exported->stream_array);
	}
	if (exported->stream_schema) {
		if (exported->stream_schema->release) {
			exported->stream_schema->release(exported->stream_schema);
		}
		free(exported->stream_schema);
	}
	if (exported->stream) {
		if (exported->stream->release) {
			exported->stream->release(exported->stream);
		}
		exported->stream = NULL;
	}
	if (exported->stream_capsule) {
		Py_DECREF(exported->stream_capsule);
	}
	if (exported->dlpack_capsule) {
		if (exported->dlpack_managed && exported->dlpack_managed->deleter) {
			exported->dlpack_managed->deleter(exported->dlpack_managed);
			exported->dlpack_managed = NULL;
		}
		Py_DECREF(exported->dlpack_capsule);
	}
	if (exported->owner) {
		Py_DECREF(exported->owner);
	}
	PyGILState_Release(gstate);
	if (exported->format) free(exported->format);
	free(exported);
}

static void* omnivm_py_exported_buffer_data(py_omnivm_exported_buffer_t* exported) {
	if (!exported) return NULL;
	if (!exported->has_buffer) return exported->data;
	Py_ssize_t min_offset = 0;
	Py_ssize_t span = 0;
	if (!omnivm_py_exported_buffer_bounds(exported, &min_offset, &span)) return NULL;
	return (void*)((char*)exported->view.buf + min_offset);
}

static void* omnivm_py_exported_buffer_validity_data(py_omnivm_exported_buffer_t* exported) {
	if (!exported || exported->has_buffer) return NULL;
	return exported->validity;
}

static int64_t omnivm_py_exported_buffer_validity_len(py_omnivm_exported_buffer_t* exported) {
	if (!exported || exported->has_buffer) return 0;
	return (int64_t)exported->validity_len;
}

static int64_t omnivm_py_exported_buffer_validity_bit_offset(py_omnivm_exported_buffer_t* exported) {
	if (!exported || exported->has_buffer) return 0;
	return (int64_t)exported->validity_bit_offset;
}

static int64_t omnivm_py_exported_buffer_null_count(py_omnivm_exported_buffer_t* exported) {
	if (!exported || exported->has_buffer) return 0;
	return (int64_t)exported->null_count;
}

static int64_t omnivm_py_exported_buffer_offset(py_omnivm_exported_buffer_t* exported) {
	if (!exported) return 0;
	if (!exported->has_buffer) return (int64_t)exported->offset;
	Py_ssize_t min_offset = 0;
	Py_ssize_t span = 0;
	if (!omnivm_py_exported_buffer_bounds(exported, &min_offset, &span)) return 0;
	return (int64_t)(-min_offset);
}

static int64_t omnivm_py_exported_buffer_len(py_omnivm_exported_buffer_t* exported) {
	if (!exported) return 0;
	Py_ssize_t len = omnivm_py_exported_buffer_span(exported);
	return (int64_t)len;
}

static int64_t omnivm_py_exported_buffer_itemsize(py_omnivm_exported_buffer_t* exported) {
	if (!exported) return 0;
	return (int64_t)(exported->has_buffer ? exported->view.itemsize : exported->itemsize);
}

static int32_t omnivm_py_exported_buffer_readonly(py_omnivm_exported_buffer_t* exported) {
	if (!exported) return 1;
	return (int32_t)(exported->has_buffer ? exported->view.readonly : exported->readonly);
}

static const char* omnivm_py_exported_buffer_format(py_omnivm_exported_buffer_t* exported) {
	if (!exported) return "";
	if (exported->has_buffer) {
		return exported->view.format ? exported->view.format : "";
	}
	return exported->format ? exported->format : "";
}

static int64_t omnivm_py_exported_buffer_ndim(py_omnivm_exported_buffer_t* exported) {
	if (!exported) return 1;
	if (exported->has_buffer) {
		if (exported->view.ndim <= 0) return 1;
		return (int64_t)exported->view.ndim;
	}
	if (exported->ndim <= 0) return 1;
	return (int64_t)exported->ndim;
}

static int64_t omnivm_py_exported_buffer_shape_at(py_omnivm_exported_buffer_t* exported, int64_t dim) {
	if (!exported || dim < 0) return 0;
	if (!exported->has_buffer) {
		if (dim < exported->ndim) return (int64_t)exported->shape[dim];
		return 0;
	}
	if (exported->view.shape && dim < exported->view.ndim) {
		return (int64_t)exported->view.shape[dim];
	}
	if (dim == 0 && exported->view.itemsize > 0) {
		return (int64_t)(exported->view.len / exported->view.itemsize);
	}
	return 0;
}

static int64_t omnivm_py_exported_buffer_stride_at(py_omnivm_exported_buffer_t* exported, int64_t dim) {
	if (!exported || dim < 0) return 0;
	if (!exported->has_buffer) {
		if (dim < exported->ndim) return (int64_t)exported->strides[dim];
		return 0;
	}
	if (exported->view.strides && dim < exported->view.ndim) {
		return (int64_t)exported->view.strides[dim];
	}
	if (exported->view.itemsize <= 0) return 0;
	int64_t ndim = omnivm_py_exported_buffer_ndim(exported);
	if (dim >= ndim) return 0;
	int64_t stride = (int64_t)exported->view.itemsize;
	for (int64_t i = ndim - 1; i > dim; i--) {
		int64_t shape = omnivm_py_exported_buffer_shape_at(exported, i);
		if (shape < 0) return 0;
		stride *= shape;
	}
	return stride;
}

// ---- Typed value bridge (Tier 2) ----

#define PY_OMNI_TAG_NULL    0
#define PY_OMNI_TAG_BOOL    1
#define PY_OMNI_TAG_I64     2
#define PY_OMNI_TAG_F64     3
#define PY_OMNI_TAG_STRING  4
#define PY_OMNI_TAG_BYTES   5
#define PY_OMNI_TAG_REF     6
#define PY_OMNI_TAG_ERROR   7

typedef struct {
    int64_t tag;
    union {
        int64_t  i;
        double   f;
        struct { char* ptr; int64_t len; } s;
        uint64_t ref;
    } v;
} py_omni_value_t;

typedef py_omni_value_t (*omni_call_typed_fn)(
    const char* runtime,
    const char* func_name,
    py_omni_value_t* args,
    int32_t nargs
);

static omni_call_typed_fn g_call_typed = NULL;

static void omnivm_py_set_typed_callback(omni_call_typed_fn fn) {
    g_call_typed = fn;
}

// Convert a Python object to omni_value_t (best-effort type detection)
static py_omni_value_t py_to_omni_value(PyObject* obj) {
    py_omni_value_t val;
    memset(&val, 0, sizeof(val));

    if (obj == Py_None) {
        val.tag = PY_OMNI_TAG_NULL;
    } else if (PyBool_Check(obj)) {
        val.tag = PY_OMNI_TAG_BOOL;
        val.v.i = (obj == Py_True) ? 1 : 0;
    } else if (PyLong_Check(obj)) {
        val.tag = PY_OMNI_TAG_I64;
        val.v.i = PyLong_AsLongLong(obj);
        if (PyErr_Occurred()) {
            PyErr_Clear();
            val.tag = PY_OMNI_TAG_ERROR;
            val.v.s.ptr = strdup("unsupported typed bridge integer: value does not fit int64");
            val.v.s.len = (int64_t)strlen(val.v.s.ptr);
        }
    } else if (PyFloat_Check(obj)) {
        val.tag = PY_OMNI_TAG_F64;
        val.v.f = PyFloat_AsDouble(obj);
    } else if (PyUnicode_Check(obj)) {
        val.tag = PY_OMNI_TAG_STRING;
        Py_ssize_t len;
        const char* utf8 = PyUnicode_AsUTF8AndSize(obj, &len);
        val.v.s.ptr = utf8 ? strndup(utf8, len) : strdup("");
        val.v.s.len = utf8 ? (int64_t)len : 0;
    } else if (PyBytes_Check(obj)) {
        val.tag = PY_OMNI_TAG_BYTES;
        char* data;
        Py_ssize_t len;
        PyBytes_AsStringAndSize(obj, &data, &len);
        val.v.s.ptr = (char*)malloc(len);
        memcpy(val.v.s.ptr, data, len);
        val.v.s.len = (int64_t)len;
    } else {
        val.tag = PY_OMNI_TAG_ERROR;
        val.v.s.ptr = strdup("unsupported typed bridge argument; complex values must cross through the manifest proxy/Arrow boundary, not implicit stringification");
        val.v.s.len = (int64_t)strlen(val.v.s.ptr);
    }
    return val;
}

// Convert omni_value_t to a Python object (new reference)
static PyObject* omni_value_to_py_with_runtime(py_omni_value_t val, const char* runtime, const char* boundary_path) {
    switch (val.tag) {
    case PY_OMNI_TAG_NULL:
        Py_RETURN_NONE;
    case PY_OMNI_TAG_BOOL:
        if (val.v.i) Py_RETURN_TRUE;
        Py_RETURN_FALSE;
    case PY_OMNI_TAG_I64:
        return PyLong_FromLongLong(val.v.i);
    case PY_OMNI_TAG_F64:
        return PyFloat_FromDouble(val.v.f);
    case PY_OMNI_TAG_STRING:
        if (val.v.s.ptr)
            return PyUnicode_FromStringAndSize(val.v.s.ptr, (Py_ssize_t)val.v.s.len);
        return PyUnicode_FromString("");
    case PY_OMNI_TAG_BYTES:
        if (val.v.s.ptr)
            return PyBytes_FromStringAndSize(val.v.s.ptr, (Py_ssize_t)val.v.s.len);
        return PyBytes_FromStringAndSize("", 0);
    case PY_OMNI_TAG_ERROR:
        if (val.v.s.ptr) {
            omnivm_py_raise_runtime_error(runtime, val.v.s.ptr, boundary_path);
        } else {
            omnivm_py_raise_runtime_error(runtime, "unknown error", boundary_path);
        }
        return NULL;
    default:
        Py_RETURN_NONE;
    }
}

static PyObject* omni_value_to_py(py_omni_value_t val) {
    return omni_value_to_py_with_runtime(val, NULL, NULL);
}

// Free C-allocated data in an omni_value_t
static void free_omni_value(py_omni_value_t* val) {
    if (val->tag == PY_OMNI_TAG_STRING || val->tag == PY_OMNI_TAG_BYTES ||
        val->tag == PY_OMNI_TAG_ERROR) {
        if (val->v.s.ptr) {
            free(val->v.s.ptr);
            val->v.s.ptr = NULL;
        }
    }
}

// Helper to get the value from a StringIO object as a strdup'd C string.
static char* get_stringio_value(PyObject* sio) {
    PyObject* getvalue = PyObject_GetAttrString(sio, "getvalue");
    if (!getvalue) return NULL;
    PyObject* py_val = PyObject_CallObject(getvalue, NULL);
    Py_DECREF(getvalue);
    if (!py_val) return NULL;
    const char* utf8 = PyUnicode_AsUTF8(py_val);
    char* result = utf8 ? strdup(utf8) : NULL;
    Py_DECREF(py_val);
    return result;
}

// Inner helper: runs Python code and captures stdout/stderr via StringIO redirect.
// Must be called with GIL held. Returns output (caller must free) or NULL on error.
// On error, returns "ERR:<traceback>" so the Go side can extract the error message.
static char* omnivm_py_exec_inner(const char* code) {
    // Set up output capture
    PyObject* io_module = PyImport_ImportModule("io");
    if (!io_module) return NULL;

    PyObject* string_io_class = PyObject_GetAttrString(io_module, "StringIO");
    Py_DECREF(io_module);
    if (!string_io_class) return NULL;

    PyObject* stdout_io = PyObject_CallObject(string_io_class, NULL);
    if (!stdout_io) { Py_DECREF(string_io_class); return NULL; }

    PyObject* stderr_io = PyObject_CallObject(string_io_class, NULL);
    Py_DECREF(string_io_class);
    if (!stderr_io) { Py_DECREF(stdout_io); return NULL; }

    // Redirect sys.stdout and sys.stderr
    PyObject* sys_module = PyImport_ImportModule("sys");
    if (!sys_module) { Py_DECREF(stdout_io); Py_DECREF(stderr_io); return NULL; }

    PyObject* old_stdout = PyObject_GetAttrString(sys_module, "stdout");
    PyObject* old_stderr = PyObject_GetAttrString(sys_module, "stderr");
    PyObject_SetAttrString(sys_module, "stdout", stdout_io);
    PyObject_SetAttrString(sys_module, "stderr", stderr_io);

    // Execute code
    int result = PyRun_SimpleString(code);

    // Restore stdout and stderr
    if (old_stdout) {
        PyObject_SetAttrString(sys_module, "stdout", old_stdout);
        Py_DECREF(old_stdout);
    }
    if (old_stderr) {
        PyObject_SetAttrString(sys_module, "stderr", old_stderr);
        Py_DECREF(old_stderr);
    }

    char* output = NULL;
    if (result == 0) {
        output = get_stringio_value(stdout_io);
    } else {
        // On error, return the captured traceback from stderr as ERR: prefix
        char* err_text = get_stringio_value(stderr_io);
        if (err_text && strlen(err_text) > 0) {
            size_t len = strlen(err_text) + 5; // "ERR:" + null
            output = (char*)malloc(len);
            if (output) {
                snprintf(output, len, "ERR:%s", err_text);
            }
            free(err_text);
        }
        // If no stderr captured, output stays NULL
    }

    Py_DECREF(stdout_io);
    Py_DECREF(stderr_io);
    Py_DECREF(sys_module);

    return output;
}

// Thread-safe exec: acquires GIL, delegates to inner, releases GIL.
// PyGILState_Ensure is recursive-safe — no-op if GIL already held.
static char* omnivm_py_exec(const char* code) {
    PyGILState_STATE gstate = PyGILState_Ensure();
    char* result = omnivm_py_exec_inner(code);
    PyGILState_Release(gstate);
    return result;
}

// Helper to convert a PyObject to a strdup'd string.
static char* py_obj_to_str(PyObject* obj) {
    if (!obj || obj == Py_None) return strdup("None");
    PyObject* str_result = PyObject_Str(obj);
    Py_DECREF(obj);
    if (!str_result) return strdup("None");
    const char* utf8 = PyUnicode_AsUTF8(str_result);
    char* output = strdup(utf8 ? utf8 : "None");
    Py_DECREF(str_result);
    return output;
}

// Inner eval: try Py_eval_input first, then multi-line split (like Jupyter).
// Must be called with GIL held. Returns expression value as string (caller must free) or NULL on error.
static char* omnivm_py_eval_inner(const char* code) {
    PyObject* main_module = PyImport_AddModule("__main__");
    if (!main_module) return NULL;
    PyObject* globals = PyModule_GetDict(main_module);

    // Try as single expression first (Py_eval_input)
    PyObject* result = PyRun_String(code, Py_eval_input, globals, globals);
    if (result) {
        return py_obj_to_str(result);
    }
    if (PyErr_ExceptionMatches(PyExc_SyntaxError)) {
        PyErr_Clear();
    } else {
        return NULL;
    }

    // Multi-line: run all-but-last line as statements, eval last line as expression.
    // Find the last non-blank line.
    const char* end = code + strlen(code);
    while (end > code && (end[-1] == '\n' || end[-1] == ' ' || end[-1] == '\t' || end[-1] == '\r'))
        end--;
    if (end == code) {
        // Empty code
        return strdup("None");
    }

    // Find start of last line
    const char* last_start = end;
    while (last_start > code && last_start[-1] != '\n')
        last_start--;

    // If last line is indented, it's part of a block — run everything as statements
    if (last_start[0] == ' ' || last_start[0] == '\t') {
        result = PyRun_String(code, Py_file_input, globals, globals);
        if (!result) return NULL;
        Py_DECREF(result);
        return strdup("None");
    }

    // If there's no preceding code, just run as statement
    if (last_start == code) {
        result = PyRun_String(code, Py_file_input, globals, globals);
        if (!result) return NULL;
        Py_DECREF(result);
        return strdup("None");
    }

    // Split: head = statements, tail = last expression
    size_t head_len = last_start - code;
    size_t tail_len = end - last_start;

    char* head = (char*)malloc(head_len + 1);
    if (!head) return NULL;
    memcpy(head, code, head_len);
    head[head_len] = '\0';

    char* tail = (char*)malloc(tail_len + 1);
    if (!tail) { free(head); return NULL; }
    memcpy(tail, last_start, tail_len);
    tail[tail_len] = '\0';

    // Run head as statements
    result = PyRun_String(head, Py_file_input, globals, globals);
    free(head);
    if (!result) {
        free(tail);
        return NULL;
    }
    Py_DECREF(result);

    // Try tail as expression
    result = PyRun_String(tail, Py_eval_input, globals, globals);
    if (result) {
        free(tail);
        return py_obj_to_str(result);
    }
    if (PyErr_ExceptionMatches(PyExc_SyntaxError)) {
        PyErr_Clear();
    } else {
        free(tail);
        return NULL;
    }

    // Last line isn't an expression, run as statement
    result = PyRun_String(tail, Py_file_input, globals, globals);
    free(tail);
    if (!result) return NULL;
    Py_DECREF(result);
    return strdup("None");
}

// Typed eval inner: returns py_omni_value_t preserving native Python types.
static py_omni_value_t omnivm_py_eval_typed_inner(const char* code) {
    py_omni_value_t null_val;
    memset(&null_val, 0, sizeof(null_val));

    PyObject* main_module = PyImport_AddModule("__main__");
    if (!main_module) {
        py_omni_value_t err;
        memset(&err, 0, sizeof(err));
        err.tag = PY_OMNI_TAG_ERROR;
        err.v.s.ptr = strdup("failed to get __main__");
        err.v.s.len = strlen(err.v.s.ptr);
        return err;
    }
    PyObject* globals = PyModule_GetDict(main_module);

    // Try as single expression first
    PyObject* result = PyRun_String(code, Py_eval_input, globals, globals);
    if (result) {
        py_omni_value_t val = py_to_omni_value(result);
        Py_DECREF(result);
        return val;
    }
    PyErr_Clear();

    // Multi-line: same split logic as omnivm_py_eval_inner
    const char* end = code + strlen(code);
    while (end > code && (end[-1] == '\n' || end[-1] == ' ' || end[-1] == '\t' || end[-1] == '\r'))
        end--;
    if (end == code) return null_val;

    const char* last_start = end;
    while (last_start > code && last_start[-1] != '\n')
        last_start--;

    if (last_start[0] == ' ' || last_start[0] == '\t' || last_start == code) {
        result = PyRun_String(code, Py_file_input, globals, globals);
        if (!result) {
            py_omni_value_t err;
            memset(&err, 0, sizeof(err));
            err.tag = PY_OMNI_TAG_ERROR;
            err.v.s.ptr = strdup("python eval error");
            err.v.s.len = strlen(err.v.s.ptr);
            return err;
        }
        Py_DECREF(result);
        return null_val;
    }

    size_t head_len = last_start - code;
    size_t tail_len = end - last_start;
    char* head = (char*)malloc(head_len + 1);
    if (!head) return null_val;
    memcpy(head, code, head_len);
    head[head_len] = '\0';
    char* tail = (char*)malloc(tail_len + 1);
    if (!tail) { free(head); return null_val; }
    memcpy(tail, last_start, tail_len);
    tail[tail_len] = '\0';

    result = PyRun_String(head, Py_file_input, globals, globals);
    free(head);
    if (!result) { free(tail); return null_val; }
    Py_DECREF(result);

    result = PyRun_String(tail, Py_eval_input, globals, globals);
    if (result) {
        free(tail);
        py_omni_value_t val = py_to_omni_value(result);
        Py_DECREF(result);
        return val;
    }
    PyErr_Clear();

    result = PyRun_String(tail, Py_file_input, globals, globals);
    free(tail);
    if (!result) return null_val;
    Py_DECREF(result);
    return null_val;
}

// Thread-safe typed eval: acquires GIL, evaluates, releases GIL.
static py_omni_value_t omnivm_py_eval_typed(const char* code) {
    PyGILState_STATE gstate = PyGILState_Ensure();
    py_omni_value_t result = omnivm_py_eval_typed_inner(code);
    PyGILState_Release(gstate);
    return result;
}

// Thread-safe eval: acquires GIL, delegates to inner, releases GIL.
static char* omnivm_py_eval(const char* code) {
    PyGILState_STATE gstate = PyGILState_Ensure();
    char* result = omnivm_py_eval_inner(code);
    PyGILState_Release(gstate);
    return result;
}

// Inner fetch error: retrieves current Python error as a string.
// Must be called with GIL held. Caller must free.
static char* omnivm_py_fetch_error_inner() {
    PyObject *type, *value, *traceback;
    PyErr_Fetch(&type, &value, &traceback);
    if (!value) {
        Py_XDECREF(type);
        Py_XDECREF(traceback);
        return NULL;
    }

    PyObject* str = PyObject_Str(value);
    PyObject* type_name = type ? PyObject_GetAttrString(type, "__name__") : NULL;
    char* result = NULL;
    if (str) {
        const char* utf8 = PyUnicode_AsUTF8(str);
        const char* type_utf8 = NULL;
        if (type_name) type_utf8 = PyUnicode_AsUTF8(type_name);
        if (utf8 && type_utf8) {
            size_t len = strlen(type_utf8) + strlen(utf8) + 3;
            result = (char*)malloc(len);
            if (result) snprintf(result, len, "%s: %s", type_utf8, utf8);
        } else if (utf8) {
            result = strdup(utf8);
        }
        Py_DECREF(str);
    }
    Py_XDECREF(type_name);

    Py_XDECREF(type);
    Py_XDECREF(value);
    Py_XDECREF(traceback);
    PyErr_Clear();
    return result;
}

// Thread-safe fetch error: acquires GIL, delegates to inner, releases GIL.
static char* omnivm_py_fetch_error() {
    PyGILState_STATE gstate = PyGILState_Ensure();
    char* result = omnivm_py_fetch_error_inner();
    PyGILState_Release(gstate);
    return result;
}

static char* omnivm_py_unicode_attr_dup(PyObject* obj, const char* name) {
    PyObject* value = PyObject_GetAttrString(obj, name);
    if (!value) {
        PyErr_Clear();
        return NULL;
    }
    if (value == Py_None) {
        Py_DECREF(value);
        return NULL;
    }
    PyObject* text = PyObject_Str(value);
    Py_DECREF(value);
    if (!text) {
        PyErr_Clear();
        return NULL;
    }
    const char* utf8 = PyUnicode_AsUTF8(text);
    char* out = (utf8 && utf8[0]) ? strdup(utf8) : NULL;
    Py_DECREF(text);
    if (!out) {
        PyErr_Clear();
    }
    return out;
}

static void omnivm_py_append_text(char** out, size_t* len, const char* text) {
    if (!text || !text[0]) return;
    size_t add = strlen(text);
    char* next = (char*)realloc(*out, *len + add + 1);
    if (!next) return;
    memcpy(next + *len, text, add + 1);
    *out = next;
    *len += add;
}

static PyObject* omnivm_py_get_validation_details(PyObject* value) {
    if (!value) return NULL;

    if (PyObject_HasAttrString(value, "details")) {
        PyObject* details = PyObject_GetAttrString(value, "details");
        if (details && details != Py_None) return details;
        Py_XDECREF(details);
        PyErr_Clear();
    }

    PyObject* details = omnivm_py_call_noarg_method(value, "errors");
    if (details) return details;

    details = omnivm_py_call_noarg_method(value, "normalized_messages");
    if (details) return details;

    static const char* attrs[] = {"messages", "message_dict", "error_dict", NULL};
    for (int i = 0; attrs[i]; ++i) {
        if (!PyObject_HasAttrString(value, attrs[i])) continue;
        details = PyObject_GetAttrString(value, attrs[i]);
        if (details && details != Py_None) return details;
        Py_XDECREF(details);
        PyErr_Clear();
    }

    return NULL;
}

static char* omnivm_py_error_details_json(PyObject* value) {
    PyObject* details = omnivm_py_get_validation_details(value);
    if (!details) return NULL;
    PyObject* wrapped = NULL;
    if (!PyMapping_Check(details)) {
        wrapped = PyDict_New();
        if (wrapped && PyDict_SetItemString(wrapped, "errors", details) != 0) {
            Py_DECREF(wrapped);
            wrapped = NULL;
            PyErr_Clear();
        }
    }
    PyObject* serializable = wrapped ? wrapped : details;

    PyObject* json_module = PyImport_ImportModule("json");
    if (!json_module) {
        Py_XDECREF(wrapped);
        Py_DECREF(details);
        PyErr_Clear();
        return NULL;
    }
    PyObject* dumps = PyObject_GetAttrString(json_module, "dumps");
    PyObject* str_type = (PyObject*)&PyUnicode_Type;
    PyObject* result = NULL;
    if (dumps) {
        PyObject* args = PyTuple_Pack(1, serializable);
        PyObject* kwargs = PyDict_New();
        if (args && kwargs && PyDict_SetItemString(kwargs, "default", str_type) == 0) {
            result = PyObject_Call(dumps, args, kwargs);
        }
        Py_XDECREF(args);
        Py_XDECREF(kwargs);
    }
    Py_XDECREF(dumps);
    Py_DECREF(json_module);
    Py_XDECREF(wrapped);
    Py_DECREF(details);
    if (!result) {
        PyErr_Clear();
        return NULL;
    }
    const char* utf8 = PyUnicode_AsUTF8(result);
    char* out = (utf8 && utf8[0]) ? strdup(utf8) : NULL;
    Py_DECREF(result);
    return out;
}

static char* omnivm_py_format_runtime_error_value(PyObject* value) {
    if (!value) return NULL;
    char* runtime = omnivm_py_unicode_attr_dup(value, "runtime");
    if (!runtime) return NULL;
    char* err_type = omnivm_py_unicode_attr_dup(value, "type");
    char* message = omnivm_py_unicode_attr_dup(value, "message");
    char* traceback = omnivm_py_unicode_attr_dup(value, "traceback");
    char* handle = omnivm_py_unicode_attr_dup(value, "original_error_handle");
    char* details_json = omnivm_py_error_details_json(value);

    char* out = NULL;
    size_t len = 0;
    omnivm_py_append_text(&out, &len, runtime);
    omnivm_py_append_text(&out, &len, ": ");
    if (err_type) {
        omnivm_py_append_text(&out, &len, err_type);
        omnivm_py_append_text(&out, &len, ": ");
    }
    if (message) {
        omnivm_py_append_text(&out, &len, message);
    }
    if (traceback) {
        omnivm_py_append_text(&out, &len, "\n");
        omnivm_py_append_text(&out, &len, traceback);
    }

    PyObject* causes = PyObject_GetAttrString(value, "cause_chain");
    if (causes && PySequence_Check(causes)) {
        Py_ssize_t n = PySequence_Size(causes);
        for (Py_ssize_t i = 0; i < n; ++i) {
            PyObject* cause = PySequence_GetItem(causes, i);
            if (!cause) {
                PyErr_Clear();
                continue;
            }
            PyObject* cause_type_obj = PyMapping_Check(cause) ? PyMapping_GetItemString(cause, "type") : NULL;
            if (!cause_type_obj) PyErr_Clear();
            PyObject* cause_msg_obj = PyMapping_Check(cause) ? PyMapping_GetItemString(cause, "message") : NULL;
            if (!cause_msg_obj) PyErr_Clear();
            char* cause_type = cause_type_obj ? omnivm_py_unicode_attr_dup(cause_type_obj, "__str__") : NULL;
            if (cause_type_obj) {
                PyObject* s = PyObject_Str(cause_type_obj);
                free(cause_type);
                cause_type = NULL;
                if (s) {
                    const char* utf8 = PyUnicode_AsUTF8(s);
                    if (utf8 && utf8[0]) cause_type = strdup(utf8);
                    Py_DECREF(s);
                } else {
                    PyErr_Clear();
                }
                Py_DECREF(cause_type_obj);
            }
            char* cause_message = NULL;
            if (cause_msg_obj) {
                PyObject* s = PyObject_Str(cause_msg_obj);
                if (s) {
                    const char* utf8 = PyUnicode_AsUTF8(s);
                    if (utf8 && utf8[0]) cause_message = strdup(utf8);
                    Py_DECREF(s);
                } else {
                    PyErr_Clear();
                }
                Py_DECREF(cause_msg_obj);
            }
            omnivm_py_append_text(&out, &len, "\nCaused by: ");
            if (cause_type) {
                omnivm_py_append_text(&out, &len, cause_type);
                omnivm_py_append_text(&out, &len, ": ");
            }
            if (cause_message) {
                omnivm_py_append_text(&out, &len, cause_message);
            }
            free(cause_type);
            free(cause_message);
            Py_DECREF(cause);
        }
    } else if (!causes) {
        PyErr_Clear();
    }
    Py_XDECREF(causes);

    if (handle) {
        omnivm_py_append_text(&out, &len, "\nOriginal error handle: ");
        omnivm_py_append_text(&out, &len, handle);
    }
    if (details_json) {
        omnivm_py_append_text(&out, &len, "\nDetails: ");
        omnivm_py_append_text(&out, &len, details_json);
    }
    free(runtime);
    free(err_type);
    free(message);
    free(traceback);
    free(handle);
    free(details_json);
    if (!out) return strdup("");
    return out;
}

// Inner fetch traceback error: retrieves current Python error including stack.
// Must be called with GIL held. Caller must free.
static char* omnivm_py_fetch_traceback_error_inner() {
    PyObject *type, *value, *traceback;
    PyErr_Fetch(&type, &value, &traceback);
    if (!type && !value) {
        Py_XDECREF(traceback);
        return NULL;
    }
    PyErr_NormalizeException(&type, &value, &traceback);

    char* result = NULL;
    result = omnivm_py_format_runtime_error_value(value);
    if (result) {
        Py_XDECREF(type);
        Py_XDECREF(value);
        Py_XDECREF(traceback);
        PyErr_Clear();
        return result;
    }

    PyObject* traceback_module = PyImport_ImportModule("traceback");
    if (traceback_module && type && value) {
        PyObject* format_exception = PyObject_GetAttrString(traceback_module, "format_exception");
        if (format_exception) {
            PyObject* tb_arg = traceback ? traceback : Py_None;
            PyObject* formatted = PyObject_CallFunctionObjArgs(format_exception, type, value, tb_arg, NULL);
            if (formatted) {
                PyObject* empty = PyUnicode_FromString("");
                if (empty) {
                    PyObject* joined = PyUnicode_Join(empty, formatted);
                    if (joined) {
                        const char* utf8 = PyUnicode_AsUTF8(joined);
                        if (utf8) result = strdup(utf8);
                        Py_DECREF(joined);
                    }
                    Py_DECREF(empty);
                }
                Py_DECREF(formatted);
            }
            Py_DECREF(format_exception);
        }
    }
    Py_XDECREF(traceback_module);

    if (result && value) {
        char* handle = omnivm_py_unicode_attr_dup(value, "original_error_handle");
        if (handle) {
            size_t len = strlen(result);
            omnivm_py_append_text(&result, &len, "\nOriginal error handle: ");
            omnivm_py_append_text(&result, &len, handle);
            free(handle);
        }
        char* details_json = omnivm_py_error_details_json(value);
        if (details_json) {
            size_t len = strlen(result);
            omnivm_py_append_text(&result, &len, "\nDetails: ");
            omnivm_py_append_text(&result, &len, details_json);
            free(details_json);
        }
    }

    if (!result) {
        PyErr_Clear();
    }

    if (!result && value) {
        PyObject* str = PyObject_Str(value);
        PyObject* type_name = type ? PyObject_GetAttrString(type, "__name__") : NULL;
        if (str) {
            const char* utf8 = PyUnicode_AsUTF8(str);
            const char* type_utf8 = NULL;
            if (type_name) type_utf8 = PyUnicode_AsUTF8(type_name);
            if (utf8 && type_utf8) {
                size_t len = strlen(type_utf8) + strlen(utf8) + 3;
                result = (char*)malloc(len);
                if (result) snprintf(result, len, "%s: %s", type_utf8, utf8);
            } else if (utf8) {
                result = strdup(utf8);
            }
            Py_DECREF(str);
        }
        Py_XDECREF(type_name);
    }

    Py_XDECREF(type);
    Py_XDECREF(value);
    Py_XDECREF(traceback);
    PyErr_Clear();
    return result;
}

// Thread-safe fetch traceback error: acquires GIL, delegates to inner, releases GIL.
static char* omnivm_py_fetch_traceback_error() {
    PyGILState_STATE gstate = PyGILState_Ensure();
    char* result = omnivm_py_fetch_traceback_error_inner();
    PyGILState_Release(gstate);
    return result;
}

// Thread-safe eval with error capture: evaluates and, on failure, fetches the
// active traceback before releasing the GIL or returning to Go. Python stores
// exceptions in thread-local state, so callers must not fetch the error in a
// separate cgo call after the Go goroutine may have moved OS threads.
static char* omnivm_py_eval_with_traceback_error(const char* code, char** error_out) {
    if (error_out) *error_out = NULL;
    PyGILState_STATE gstate = PyGILState_Ensure();
    char* result = omnivm_py_eval_inner(code);
    if (!result && error_out) {
        *error_out = omnivm_py_fetch_traceback_error_inner();
    }
    PyGILState_Release(gstate);
    return result;
}

// C implementation of omnivm.call(runtime, code) for Python.
// Releases GIL during the cross-runtime call so other threads can run Python
// concurrently. This also prevents deadlock when a foreign thread holds the
// GIL and calls into a runtime that needs to pump Python.
static PyObject* py_omnivm_call(PyObject* self, PyObject* args) {
    const char* runtime;
    const char* code;
    if (!PyArg_ParseTuple(args, "ss", &runtime, &code)) {
        return NULL;
    }

    if (!g_bridge_call) {
        PyErr_SetString(PyExc_RuntimeError, "omnivm bridge not initialized");
        return NULL;
    }

    // Release GIL while calling into other runtime.
    // Between SaveThread and RestoreThread, NO Python C-API calls are allowed.
    PyThreadState* _save = PyEval_SaveThread();
    char* result = g_bridge_call(runtime, code);
    PyEval_RestoreThread(_save);

    if (!result) {
        PyErr_SetString(PyExc_RuntimeError, "omnivm.call returned NULL");
        return NULL;
    }

    // Check for error prefix
    if (strncmp(result, "ERR:", 4) == 0) {
        char boundary[256];
        snprintf(boundary, sizeof(boundary), "call[%s]", runtime ? runtime : "");
        omnivm_py_raise_runtime_error(runtime, result + 4, boundary);
        if (g_bridge_free) g_bridge_free(result);
        return NULL;
    }

    PyObject* py_result = PyUnicode_FromString(result);
    if (g_bridge_free) g_bridge_free(result);
    return py_result;
}

// py_omnivm_get_buffer(name) -> buffer-protocol object or None
// Returns a zero-copy view wrapping the shared buffer's data.
static PyObject* py_omnivm_get_buffer(PyObject* self, PyObject* args) {
    const char* name;
    if (!PyArg_ParseTuple(args, "s", &name)) return NULL;

    if (!g_buf_get) {
        PyErr_SetString(PyExc_RuntimeError, "omnivm buffer bridge not initialized");
        return NULL;
    }

    py_omni_buffer_t buf;
    memset(&buf, 0, sizeof(buf));
    int rc = g_buf_get(name, &buf);
    if (rc != 0) {
        Py_RETURN_NONE;
    }

    if (buf.data == NULL || buf.len <= 0) {
        if (g_buf_release) {
            g_buf_release(name);
        }
        return PyBytes_FromStringAndSize("", 0);
    }

	// Return a private buffer-protocol view. Its deallocator releases the
	// OmniVM borrow when Python drops the last reference.
	return py_omnivm_memoryview_from_buffer(name, &buf);
}

// py_omnivm_set_buffer(name, data, dtype=0) -> None
// Copies the buffer-protocol object into the shared store.
static PyObject* py_omnivm_set_buffer(PyObject* self, PyObject* args) {
    const char* name;
    Py_buffer view;
    int dtype = 0; // default: bytes
    if (!PyArg_ParseTuple(args, "sy*|i", &name, &view, &dtype)) return NULL;

    if (!g_buf_set) {
        PyBuffer_Release(&view);
        PyErr_SetString(PyExc_RuntimeError, "omnivm buffer bridge not initialized");
        return NULL;
    }

    py_omni_buffer_t buf;
    buf.data = view.buf;
    buf.len = (int64_t)view.len;
    buf.dtype = (int32_t)dtype;
    buf.owned = 0; // Go side will copy
    buf.read_only = view.readonly ? 1 : 0;

    int rc = g_buf_set(name, buf);
    PyBuffer_Release(&view);

    if (rc != 0) {
        PyErr_SetString(PyExc_RuntimeError, "omnivm.set_buffer failed");
        return NULL;
    }
    Py_RETURN_NONE;
}

// py_omnivm_release_buffer(name) -> None
// Schedule a deferred release of a named buffer.
static PyObject* py_omnivm_release_buffer(PyObject* self, PyObject* args) {
    const char* name;
    if (!PyArg_ParseTuple(args, "s", &name)) return NULL;

    if (!g_buf_release) {
        PyErr_SetString(PyExc_RuntimeError, "omnivm buffer bridge not initialized");
        return NULL;
    }

    g_buf_release(name);
    Py_RETURN_NONE;
}

// Method table for the omnivm module
// py_omnivm_call_typed(runtime, func_name, *args) -> value
// Calls a function in another runtime using typed values (no JSON).
static PyObject* py_omnivm_call_typed(PyObject* self, PyObject* args) {
    const char* runtime;
    const char* func_name;
    PyObject* py_args_tuple;

    // Parse: (str, str, tuple)
    if (!PyArg_ParseTuple(args, "ssO", &runtime, &func_name, &py_args_tuple)) {
        return NULL;
    }

    if (!g_call_typed) {
        PyErr_SetString(PyExc_RuntimeError, "omnivm typed bridge not initialized");
        return NULL;
    }

    if (!PyTuple_Check(py_args_tuple) && !PyList_Check(py_args_tuple)) {
        PyErr_SetString(PyExc_TypeError, "third argument must be a tuple or list");
        return NULL;
    }

    Py_ssize_t nargs = PySequence_Size(py_args_tuple);
    py_omni_value_t* c_args = NULL;
    if (nargs > 0) {
        c_args = (py_omni_value_t*)calloc(nargs, sizeof(py_omni_value_t));
        for (Py_ssize_t i = 0; i < nargs; i++) {
            PyObject* item = PySequence_GetItem(py_args_tuple, i);
            c_args[i] = py_to_omni_value(item);
            Py_DECREF(item);
            if (c_args[i].tag == PY_OMNI_TAG_ERROR) {
                PyErr_SetString(PyExc_TypeError, c_args[i].v.s.ptr ? c_args[i].v.s.ptr : "unsupported typed bridge argument");
                for (Py_ssize_t j = 0; j <= i; j++) {
                    free_omni_value(&c_args[j]);
                }
                free(c_args);
                return NULL;
            }
        }
    }

    // Release GIL while calling into other runtime
    PyThreadState* _save = PyEval_SaveThread();
    py_omni_value_t result = g_call_typed(runtime, func_name, c_args, (int32_t)nargs);
    PyEval_RestoreThread(_save);

    // Free args
    if (c_args) {
        for (int32_t i = 0; i < nargs; i++) {
            free_omni_value(&c_args[i]);
        }
        free(c_args);
    }

    // Convert result to Python
    char boundary[256];
    snprintf(boundary, sizeof(boundary), "call_typed[%s]", runtime ? runtime : "");
    PyObject* py_result = omni_value_to_py_with_runtime(result, runtime, boundary);
    free_omni_value(&result);
    return py_result;
}

static PyMethodDef omnivm_methods[] = {
    {"call", py_omnivm_call, METH_VARARGS, "Call another runtime: omnivm.call(runtime, code)"},
    {"call_typed", py_omnivm_call_typed, METH_VARARGS, "Call a function with typed args: omnivm.call_typed(runtime, func, (args,))"},
	{"get_buffer", py_omnivm_get_buffer, METH_VARARGS, "Get a shared buffer: omnivm.get_buffer(name) -> buffer|None"},
    {"set_buffer", py_omnivm_set_buffer, METH_VARARGS, "Set a shared buffer: omnivm.set_buffer(name, data, dtype=0)"},
    {"release_buffer", py_omnivm_release_buffer, METH_VARARGS, "Release a shared buffer: omnivm.release_buffer(name)"},
    {NULL, NULL, 0, NULL}
};

// Module definition
static struct PyModuleDef omnivm_module_def = {
    PyModuleDef_HEAD_INIT,
    "omnivm",
    "OmniVM cross-runtime bridge",
    -1,
    omnivm_methods
};

// Register the omnivm module
static void omnivm_py_register_bridge() {
    if (PyType_Ready(&py_omnivm_buffer_view_type) < 0) {
        PyErr_Clear();
        return;
    }
    PyObject* module = PyModule_Create(&omnivm_module_def);
    if (module) {
        PyObject* sys_modules = PySys_GetObject("modules");
        if (sys_modules) {
            PyDict_SetItemString(sys_modules, "omnivm", module);
        }
        Py_DECREF(module);
    }
}

static void omnivm_py_set_bridge_callback(omni_call_fn call_fn, omni_free_fn free_fn) {
    g_bridge_call = call_fn;
    g_bridge_free = free_fn;
}

static void omnivm_py_set_bridge_free(omni_free_fn free_fn) {
    g_bridge_free = free_fn;
}

// Pipe-based interrupt mechanism.
// PyErr_SetInterrupt() from a non-Python thread (cgo) fails because
// _PyRuntimeState_GetThreadState() returns NULL, so eval_breaker is
// never set. Instead, we use a pipe: Go writes a byte, a Python daemon
// thread reads it and calls _thread.interrupt_main() which has a proper
// thread state and correctly triggers eval_breaker.
static int interrupt_pipe[2] = {-1, -1};

// Create the interrupt pipe and start a Python daemon thread that waits
// for bytes and calls _thread.interrupt_main(). Must be called after
// Py_InitializeEx and signal handler setup.
static void omnivm_py_setup_interrupt(void) {
    if (interrupt_pipe[0] >= 0 && interrupt_pipe[1] >= 0) return;
    if (pipe(interrupt_pipe) != 0) return;
    char code[512];
    snprintf(code, sizeof(code),
        "import threading, os, _thread\n"
        "def _omnivm_interrupt_watcher():\n"
        "    while True:\n"
        "        os.read(%d, 1)\n"
        "        _thread.interrupt_main()\n"
        "_t = threading.Thread(target=_omnivm_interrupt_watcher, daemon=True)\n"
        "_t.start()\n",
        interrupt_pipe[0]);
    PyRun_SimpleString(code);
}

// Signal the Python interrupt watcher thread. Safe from any thread,
// no GIL or thread state required — just a write() to a pipe fd.
static void omnivm_py_interrupt(void) {
    if (interrupt_pipe[1] >= 0) {
        char c = 1;
        (void)write(interrupt_pipe[1], &c, 1);
    }
}

// Drain any stale interrupt: empty the pipe, wait for the watcher thread
// to finish processing any byte it already read, then absorb any pending
// KeyboardInterrupt. Call this before code that must not be interrupted
// by a prior test's leftover interrupt.
static void omnivm_py_clear_interrupt(void) {
    // Drain unread bytes from the pipe (non-blocking) — no GIL needed
    if (interrupt_pipe[0] >= 0) {
        char buf[16];
        int flags = fcntl(interrupt_pipe[0], F_GETFL, 0);
        fcntl(interrupt_pipe[0], F_SETFL, flags | O_NONBLOCK);
        while (read(interrupt_pipe[0], buf, sizeof(buf)) > 0) {}
        fcntl(interrupt_pipe[0], F_SETFL, flags);
    }
    // Wait for watcher thread to process any byte it already read
    usleep(10000); // 10ms
    // Absorb any KeyboardInterrupt already queued by the watcher thread
    // Needs GIL since we call Python C-API.
    PyGILState_STATE gstate = PyGILState_Ensure();
    PyRun_SimpleString("try:\n pass\nexcept KeyboardInterrupt:\n pass");
    PyErr_Clear();
    PyGILState_Release(gstate);
}

// Return a function pointer to omnivm_py_interrupt for the watchdog.
static void* omnivm_py_get_interrupt_ptr(void) {
	return (void*)omnivm_py_interrupt;
}

// Fork guard: fork() after JVM/Ruby init leaves dead threads holding mutexes.
// Install a pthread_atfork child handler that kills the child immediately.
// Conditional: only fires if JVM or Ruby were loaded (Go+JS are fork-safe
// when initialized post-fork).
static int fork_guard_active = 0;
static int fork_guard_installed = 0;

static void omnivm_fork_child_handler(void) {
    if (!fork_guard_active) return;

    const char* msg = "FATAL: fork() in OmniVM polyglot process. "
                      "JVM/Ruby threads do not survive fork(). "
                      "Use multiprocessing.set_start_method('spawn').\n";
    write(STDERR_FILENO, msg, strlen(msg));

    // Log C call stack to help identify the offending fork() call site.
    // backtrace() is async-signal-safe enough for a dying child process.
#ifdef __GLIBC__
    const char* hdr = "Fork call stack (child process, pre-_exit):\n";
    write(STDERR_FILENO, hdr, strlen(hdr));
    void* frames[32];
    int n = backtrace(frames, 32);
    backtrace_symbols_fd(frames, n, STDERR_FILENO);
#endif

    // Also dump Python stack if possible — this is the most likely culprit.
    // We're in the forked child so the GIL state is undefined, but
    // Py_IsInitialized() and faulthandler are safe enough pre-_exit.
    if (Py_IsInitialized()) {
        const char* py_hdr = "Python stack at fork:\n";
        write(STDERR_FILENO, py_hdr, strlen(py_hdr));
        // faulthandler.dump_traceback writes directly to fd, no GIL needed
        PyRun_SimpleString(
            "import faulthandler; faulthandler.dump_traceback(open(2,'w'))");
    }

    _exit(71);
}

static void omnivm_install_fork_guard(void) {
    if (fork_guard_installed) return;
    pthread_atfork(NULL, NULL, omnivm_fork_child_handler);
    fork_guard_installed = 1;
}

static void omnivm_activate_fork_guard(void) {
    fork_guard_active = 1;
}

// -------------------------------------------------------------------
// Python interpreter mode: _omnivm built-in C extension module
// -------------------------------------------------------------------
// Registered via PyImport_AppendInittab BEFORE Py_BytesMain() so that
// "import omnivm" works in any Python code run by the interpreter.
// Defers actual runtime init to omnivm.init_runtimes().

// Callbacks into Go (set by the main binary via OmniSetPythonModeCallbacks)
typedef char* (*omni_init_runtimes_fn)(const char* list);
typedef char* (*omni_load_plugin_fn)(const char* runtime, const char* path);
typedef void  (*omni_shutdown_fn)(void);

static omni_init_runtimes_fn  g_init_runtimes = NULL;
static omni_load_plugin_fn    g_load_plugin   = NULL;
static omni_shutdown_fn       g_shutdown      = NULL;

static void omnivm_set_pymode_callbacks(
    omni_init_runtimes_fn init_fn,
    omni_load_plugin_fn   load_fn,
    omni_shutdown_fn      shut_fn
) {
    g_init_runtimes = init_fn;
    g_load_plugin   = load_fn;
    g_shutdown      = shut_fn;
}

// omnivm.init_runtimes(["go", "javascript"])
static PyObject* pymode_init_runtimes(PyObject* self, PyObject* args) {
    PyObject* list;
    if (!PyArg_ParseTuple(args, "O!", &PyList_Type, &list)) return NULL;

    if (!g_init_runtimes) {
        PyErr_SetString(PyExc_RuntimeError, "omnivm: not running in OmniVM interpreter mode");
        return NULL;
    }

    // Build comma-separated string from list
    Py_ssize_t n = PyList_Size(list);
    char buf[256] = {0};
    int pos = 0;
    for (Py_ssize_t i = 0; i < n && pos < 250; i++) {
        PyObject* item = PyList_GetItem(list, i);
        const char* s = PyUnicode_AsUTF8(item);
        if (!s) return NULL;
        if (i > 0) buf[pos++] = ',';
        int len = strlen(s);
        if (pos + len >= 255) break;
        memcpy(buf + pos, s, len);
        pos += len;
    }
    buf[pos] = '\0';

    // Release GIL during Go runtime initialization
    PyThreadState* _save = PyEval_SaveThread();
    char* result = g_init_runtimes(buf);
    PyEval_RestoreThread(_save);

    if (result && strncmp(result, "ERR:", 4) == 0) {
        omnivm_py_raise_runtime_error(NULL, result + 4, "init_runtimes");
        if (g_bridge_free) g_bridge_free(result);
        return NULL;
    }
    if (result && g_bridge_free) g_bridge_free(result);
    Py_RETURN_NONE;
}

// omnivm.call(runtime, code)
// Reuses g_bridge_call which is set by init_runtimes via SetBridgeCallback.
static PyObject* pymode_call(PyObject* self, PyObject* args) {
    return py_omnivm_call(self, args);
}

// omnivm.load_plugin(runtime, path)
static PyObject* pymode_load_plugin(PyObject* self, PyObject* args) {
    const char* runtime;
    const char* path;
    if (!PyArg_ParseTuple(args, "ss", &runtime, &path)) return NULL;

    if (!g_load_plugin) {
        PyErr_SetString(PyExc_RuntimeError, "omnivm: not running in OmniVM interpreter mode");
        return NULL;
    }

    PyThreadState* _save = PyEval_SaveThread();
    char* result = g_load_plugin(runtime, path);
    PyEval_RestoreThread(_save);

    if (result && strncmp(result, "ERR:", 4) == 0) {
        char boundary[256];
        snprintf(boundary, sizeof(boundary), "load_plugin[%s]", runtime ? runtime : "");
        omnivm_py_raise_runtime_error(runtime, result + 4, boundary);
        if (g_bridge_free) g_bridge_free(result);
        return NULL;
    }
    if (result && g_bridge_free) g_bridge_free(result);
    Py_RETURN_NONE;
}

// omnivm.shutdown()
static PyObject* pymode_shutdown(PyObject* self, PyObject* args) {
    if (g_shutdown) {
        PyThreadState* _save = PyEval_SaveThread();
        g_shutdown();
        PyEval_RestoreThread(_save);
    }
    Py_RETURN_NONE;
}

// omnivm.execute(runtime, code) — runs code, returns captured stdout
static PyObject* pymode_execute(PyObject* self, PyObject* args) {
    const char* runtime;
    const char* code;
    if (!PyArg_ParseTuple(args, "ss", &runtime, &code)) return NULL;

    if (!g_bridge_call) {
        PyErr_SetString(PyExc_RuntimeError, "omnivm bridge not initialized — call init_runtimes() first");
        return NULL;
    }

    // For execute, we use the same bridge but with a different Go-side handler.
    // The bridge's OmniCall uses Eval(); for stdout capture we'd need Execute().
    // For now, delegate to the same call path — execute vs eval distinction
    // is handled at the Go level by the caller's code string.
    return py_omnivm_call(self, args);
}

static PyMethodDef omnivm_pymode_methods[] = {
    {"init_runtimes", pymode_init_runtimes, METH_VARARGS, "Initialize runtimes: omnivm.init_runtimes(['go', 'javascript'])"},
    {"call",          pymode_call,          METH_VARARGS, "Call a runtime: omnivm.call('go', 'expr')"},
    {"call_typed",    py_omnivm_call_typed, METH_VARARGS, "Call a function with typed args: omnivm.call_typed(runtime, func, (args,))"},
    {"load_plugin",   pymode_load_plugin,   METH_VARARGS, "Load a plugin: omnivm.load_plugin('go', '/path/to/plugin.so')"},
    {"shutdown",      pymode_shutdown,       METH_NOARGS,  "Shut down runtimes"},
    {"execute",       pymode_execute,        METH_VARARGS, "Execute code: omnivm.execute('javascript', 'code')"},
    {NULL, NULL, 0, NULL}
};

static struct PyModuleDef omnivm_pymode_module_def = {
    PyModuleDef_HEAD_INIT,
    "omnivm",
    "OmniVM polyglot runtime — call Go, JavaScript, and other runtimes from Python",
    -1,
    omnivm_pymode_methods
};

// Called by PyImport_AppendInittab registration. This is the init function
// for "import omnivm" when running in Python interpreter mode.
PyMODINIT_FUNC PyInit_omnivm(void) {
    PyObject* mod = PyModule_Create(&omnivm_pymode_module_def);
    if (!mod) return NULL;

    PyObject* dict = PyModule_GetDict(mod);
    PyObject* runtime_error_def = dict ? PyRun_String(omnivm_py_runtime_error_code, Py_file_input, dict, dict) : NULL;
    if (runtime_error_def) {
        Py_DECREF(runtime_error_def);
    } else {
        PyErr_Clear();
        PyObject* base = PyExc_RuntimeError;
        PyObject* exc = PyErr_NewException("omnivm.RuntimeError", base, NULL);
        if (exc) {
            PyModule_AddObject(mod, "RuntimeError", exc);
        }
    }

    return mod;
}
*/
import "C"
import (
	"fmt"
	"math"
	"strings"
	"unsafe"

	"github.com/omnivm/omnivm/pkg"
	"github.com/omnivm/omnivm/pkg/arrow"
	"github.com/omnivm/omnivm/pkg/polyglot"
)

// Pre-allocated C strings to avoid repeated malloc in hot paths.
// These are allocated once and never freed (process-lifetime).
var (
	cImportOmnivm   *C.char
	cSetupInterrupt *C.char
	cPumpCode       *C.char
	cForceSpawnMode *C.char
)

func init() {
	cImportOmnivm = C.CString("import omnivm")
	// Install Python's default SIGINT handler so _thread.interrupt_main() works.
	// Py_InitializeEx(0) skips signal handler setup, leaving the handler table
	// empty. Without this, _thread.interrupt_main() has no handler to invoke.
	cSetupInterrupt = C.CString(`
import signal
try:
    signal.signal(signal.SIGINT, signal.default_int_handler)
except ValueError:
    pass
`)
	cForceSpawnMode = C.CString("import multiprocessing; multiprocessing.set_start_method('spawn', force=True)")
	cPumpCode = C.CString(`
import asyncio
__omni_pump_seen = set()
def __omni_pump_loop_once(loop):
    if loop is None or id(loop) in __omni_pump_seen:
        return
    __omni_pump_seen.add(id(loop))
    if not loop.is_running() and not loop.is_closed():
        loop.call_soon(loop.stop)
        loop.run_forever()
try:
    __omni_pump_loop_once(asyncio.get_event_loop())
except RuntimeError:
    pass
for __omni_pump_obj in list(globals().values()):
    if isinstance(__omni_pump_obj, asyncio.AbstractEventLoop):
        __omni_pump_loop_once(__omni_pump_obj)
`)
}

// cpythonInitialized guards against double CPython init across Runtime instances.
// CPython can only be initialized once per process; a second call crashes on 3.14+.
var cpythonInitialized bool

// Runtime implements pkg.Runtime for CPython.
type Runtime struct {
	initialized bool
}

// New creates a new Python runtime (not yet initialized).
func New() *Runtime {
	return &Runtime{}
}

func (r *Runtime) Name() string { return "python" }

// Initialize starts CPython with signal handlers disabled.
// Must be called on the Golden Thread.
func (r *Runtime) Initialize() error {
	if r.initialized {
		return fmt.Errorf("python: already initialized")
	}

	if cpythonInitialized {
		// CPython was already initialized (and never truly finalized).
		// Install host-mode hooks for c-shared hosts, then mark this Runtime
		// as initialized - the interpreter is still live.
		gstate := C.PyGILState_Ensure()
		C.PyRun_SimpleString(cSetupInterrupt)
		C.omnivm_py_setup_interrupt()
		C.PyGILState_Release(gstate)
		C.omnivm_install_fork_guard()
		r.initialized = true
		return nil
	}

	// 0 = skip signal handler registration (Go owns signals)
	C.Py_InitializeEx(0)

	if C.Py_IsInitialized() == 0 {
		return fmt.Errorf("python: Py_InitializeEx failed")
	}

	// Register the omnivm Python module and import it into __main__
	C.omnivm_py_register_bridge()
	C.PyRun_SimpleString(cImportOmnivm)

	// Install Python's default SIGINT handler so _thread.interrupt_main() works.
	C.PyRun_SimpleString(cSetupInterrupt)

	// Set up pipe-based interrupt: a daemon thread reads from a pipe and
	// calls _thread.interrupt_main(). This lets Go's Interrupt() work from
	// any goroutine without needing the GIL or a Python thread state.
	C.omnivm_py_setup_interrupt()

	// Install fork guard: child processes created by fork() in a polyglot
	// process with JVM threads will deadlock. Kill them immediately.
	// In Go-hosted mode, activate immediately (JVM/Ruby may be loaded later).
	C.omnivm_install_fork_guard()
	C.omnivm_activate_fork_guard()

	// Force multiprocessing to use 'spawn' instead of 'fork'.
	// fork() after JVM init leaves dead threads holding mutexes.
	C.PyRun_SimpleString(cForceSpawnMode)

	// Release the GIL so it's available for all threads (including the
	// Golden Thread). Every subsequent Python call acquires/releases the
	// GIL via PyGILState_Ensure/Release. Without this, the main thread
	// holds the GIL forever and foreign threads deadlock on Ensure().
	C.PyEval_SaveThread()

	r.initialized = true
	cpythonInitialized = true
	return nil
}

// Execute runs Python code synchronously on the current thread.
// Must be called on the Golden Thread.
func (r *Runtime) Execute(code string) pkg.Result {
	if !r.initialized {
		return pkg.Result{Err: fmt.Errorf("python: not initialized")}
	}

	cCode := C.CString(code)
	defer C.free(unsafe.Pointer(cCode))

	cOutput := C.omnivm_py_exec(cCode)

	if cOutput == nil {
		// Check for Python error
		cErr := C.omnivm_py_fetch_error()
		if cErr != nil {
			errStr := C.GoString(cErr)
			C.free(unsafe.Pointer(cErr))
			return pkg.Result{Err: fmt.Errorf("python: %s", errStr)}
		}
		return pkg.Result{Err: fmt.Errorf("python: execution failed")}
	}

	output := C.GoString(cOutput)
	C.free(unsafe.Pointer(cOutput))

	// Check for ERR: prefix (captured traceback from stderr)
	if len(output) > 4 && output[:4] == "ERR:" {
		return pkg.Result{Err: fmt.Errorf("%s", output[4:])}
	}

	return pkg.Result{Output: output}
}

// Eval evaluates a Python expression and returns its value directly.
// Uses two-pass: try Py_eval_input first, fall back to Py_file_input.
func (r *Runtime) Eval(code string) pkg.Result {
	if !r.initialized {
		return pkg.Result{Err: fmt.Errorf("python: not initialized")}
	}

	cCode := C.CString(code)
	defer C.free(unsafe.Pointer(cCode))

	var cErr *C.char
	cOutput := C.omnivm_py_eval_with_traceback_error(cCode, &cErr)

	if cOutput == nil {
		if cErr != nil {
			errStr := C.GoString(cErr)
			C.free(unsafe.Pointer(cErr))
			return pkg.Result{Err: fmt.Errorf("python: %s", errStr)}
		}
		return pkg.Result{Err: fmt.Errorf("python: eval failed")}
	}

	value := C.GoString(cOutput)
	C.free(unsafe.Pointer(cOutput))
	return pkg.Result{Value: value, Output: value}
}

// ExportBuffer publishes Python-owned buffer memory into OmniVM's shared data
// plane without copying. It is intentionally generic: any contiguous or
// strided object accepted by PyObject_GetBuffer, or any strided
// __arrow_c_array__, one-chunk __arrow_c_stream__, __dlpack__,
// __array_interface__, or __array__ producer, can participate.
func (r *Runtime) ExportBuffer(name, expr string) (pkg.ExportedBuffer, bool, error) {
	if !r.initialized {
		return pkg.ExportedBuffer{}, false, fmt.Errorf("python: not initialized")
	}
	cExpr := C.CString(expr)
	defer C.free(unsafe.Pointer(cExpr))

	exported := C.omnivm_py_export_buffer(cExpr)
	if exported == nil {
		return pkg.ExportedBuffer{}, false, nil
	}

	byteLen := int64(C.omnivm_py_exported_buffer_len(exported))
	itemSize := int64(C.omnivm_py_exported_buffer_itemsize(exported))
	format := C.GoString(C.omnivm_py_exported_buffer_format(exported))
	dtype, arrowFormat, ok := pythonBufferArrowType(format, itemSize)
	if !ok || byteLen < 0 || itemSize <= 0 {
		C.omnivm_py_release_exported_buffer(exported)
		return pkg.ExportedBuffer{}, false, nil
	}

	ptr := unsafe.Pointer(C.omnivm_py_exported_buffer_data(exported))
	if byteLen > 0 && ptr == nil {
		C.omnivm_py_release_exported_buffer(exported)
		return pkg.ExportedBuffer{}, false, fmt.Errorf("python: exported buffer %q has nil data", name)
	}
	nullCount := int64(C.omnivm_py_exported_buffer_null_count(exported))
	validityLen := int64(C.omnivm_py_exported_buffer_validity_len(exported))
	validityPtr := unsafe.Pointer(C.omnivm_py_exported_buffer_validity_data(exported))
	validityBitOffset := int64(C.omnivm_py_exported_buffer_validity_bit_offset(exported))
	if nullCount < 0 || validityLen < 0 || validityBitOffset < 0 || (nullCount > 0 && validityPtr == nil) {
		C.omnivm_py_release_exported_buffer(exported)
		return pkg.ExportedBuffer{}, false, nil
	}
	offset := int64(C.omnivm_py_exported_buffer_offset(exported))
	if offset < 0 || offset >= byteLen+itemSize {
		C.omnivm_py_release_exported_buffer(exported)
		return pkg.ExportedBuffer{}, false, nil
	}
	fallbackElements := int64(0)
	if byteLen%itemSize == 0 {
		fallbackElements = byteLen / itemSize
	}
	shape, strides := pythonExportedBufferShape(exported, fallbackElements)
	elements, ok := pythonShapeProduct(shape)
	if !ok || elements < 0 {
		C.omnivm_py_release_exported_buffer(exported)
		return pkg.ExportedBuffer{}, false, nil
	}
	meta := arrow.BufferMetadata{
		Dtype:             dtype,
		Format:            arrowFormat,
		Shape:             shape,
		Strides:           strides,
		Offset:            offset,
		NullCount:         nullCount,
		ValidityBytes:     validityLen,
		ValidityBitOffset: validityBitOffset,
		ReadOnly:          C.omnivm_py_exported_buffer_readonly(exported) != 0,
		Ownership:         "producer",
	}
	if _, err := arrow.GlobalStore().SetExternalArrowWithMetadata(name, ptr, byteLen, validityPtr, validityLen, meta, func() error {
		C.omnivm_py_release_exported_buffer(exported)
		return nil
	}); err != nil {
		C.omnivm_py_release_exported_buffer(exported)
		return pkg.ExportedBuffer{}, false, err
	}
	return pkg.ExportedBuffer{
		Name:        name,
		Dtype:       dtype,
		ArrowFormat: arrowFormat,
		Elements:    elements,
		Shape:       append([]int64(nil), shape...),
		Strides:     append([]int64(nil), strides...),
		Offset:      offset,
		NullCount:   nullCount,
		ReadOnly:    meta.ReadOnly,
	}, true, nil
}

func pythonShapeProduct(shape []int64) (int64, bool) {
	product := int64(1)
	for _, dim := range shape {
		if dim < 0 {
			return 0, false
		}
		if dim == 0 {
			return 0, true
		}
		if product > math.MaxInt64/dim {
			return 0, false
		}
		product *= dim
	}
	return product, true
}

func pythonExportedBufferShape(exported *C.py_omnivm_exported_buffer_t, elements int64) ([]int64, []int64) {
	ndim := int64(C.omnivm_py_exported_buffer_ndim(exported))
	if ndim <= 0 {
		if elements < 0 {
			return nil, nil
		}
		return []int64{elements}, nil
	}
	shape := make([]int64, 0, ndim)
	strides := make([]int64, 0, ndim)
	for i := int64(0); i < ndim; i++ {
		dim := int64(C.omnivm_py_exported_buffer_shape_at(exported, C.int64_t(i)))
		if dim < 0 {
			return []int64{elements}, nil
		}
		shape = append(shape, dim)
		stride := int64(C.omnivm_py_exported_buffer_stride_at(exported, C.int64_t(i)))
		if stride != 0 {
			strides = append(strides, stride)
		}
	}
	if len(strides) != len(shape) {
		strides = nil
	}
	return shape, strides
}

func pythonBufferArrowType(format string, itemSize int64) (int32, string, bool) {
	format = strings.TrimSpace(format)
	if format != "" {
		switch format[0] {
		case '@', '=', '|':
			format = format[1:]
		case '<':
			if itemSize > 1 && !pythonHostIsLittleEndian() {
				return 0, "", false
			}
			format = format[1:]
		case '>', '!':
			if itemSize > 1 && pythonHostIsLittleEndian() {
				return 0, "", false
			}
			format = format[1:]
		}
	}
	if format == "" {
		if itemSize == 1 {
			return arrow.DtypeBytes, "C", true
		}
		return 0, "", false
	}
	if len(format) > 1 {
		format = format[len(format)-1:]
	}
	switch format {
	case "c":
		if itemSize == 1 {
			return arrow.DtypeBytes, "C", true
		}
	case "b":
		if itemSize == 1 {
			return arrow.DtypeI8, "c", true
		}
	case "B":
		if itemSize == 1 {
			return arrow.DtypeU8, "C", true
		}
	case "h":
		if itemSize == 2 {
			return arrow.DtypeI16, "s", true
		}
	case "H":
		if itemSize == 2 {
			return arrow.DtypeU16, "S", true
		}
	case "i":
		if itemSize == 4 {
			return arrow.DtypeI32, "i", true
		}
	case "I":
		if itemSize == 4 {
			return arrow.DtypeU32, "I", true
		}
	case "q", "l":
		if itemSize == 8 {
			return arrow.DtypeI64, "l", true
		}
	case "Q", "L":
		if itemSize == 8 {
			return arrow.DtypeU64, "L", true
		}
	case "f":
		if itemSize == 4 {
			return arrow.DtypeF32, "f", true
		}
	case "d", "g":
		if itemSize == 8 {
			return arrow.DtypeF64, "g", true
		}
	}
	return 0, "", false
}

func pythonHostIsLittleEndian() bool {
	var marker uint16 = 1
	return *(*byte)(unsafe.Pointer(&marker)) == 1
}

// EvalTyped evaluates Python code and returns a typed polyglot.Value.
func (r *Runtime) EvalTyped(code string) polyglot.Value {
	if !r.initialized {
		return polyglot.Error("python: not initialized")
	}
	cCode := C.CString(code)
	defer C.free(unsafe.Pointer(cCode))

	cResult := C.omnivm_py_eval_typed(cCode)
	ptr := unsafe.Pointer(&cResult)
	val := polyglot.FromCValueRaw(ptr)
	polyglot.FreeCValueRaw(ptr)
	return val
}

// SetBridgeCallback installs the cross-runtime callback function pointer.
func (r *Runtime) SetBridgeCallback(callPtr, freePtr uintptr) {
	C.omnivm_py_set_bridge_callback(
		C.omni_call_fn(unsafe.Pointer(callPtr)),
		C.omni_free_fn(unsafe.Pointer(freePtr)),
	)
}

// SetBufCallbacks installs the buffer bridge function pointers.
func (r *Runtime) SetBufCallbacks(getPtr, setPtr, releasePtr uintptr) {
	C.omnivm_py_set_buf_callbacks(
		C.omni_buf_get_fn(unsafe.Pointer(getPtr)),
		C.omni_buf_set_fn(unsafe.Pointer(setPtr)),
		C.omni_buf_release_fn(unsafe.Pointer(releasePtr)),
	)
}

// SetTypedCallback installs the typed call bridge function pointer.
func (r *Runtime) SetTypedCallback(ptr uintptr) {
	C.omnivm_py_set_typed_callback(
		C.omni_call_typed_fn(unsafe.Pointer(ptr)),
	)
}

// ExecuteOffThread runs Python code on a separate OS thread.
// GIL acquisition is handled automatically by omnivm_py_exec (Phase 1A).
func (r *Runtime) ExecuteOffThread(code string) <-chan pkg.Result {
	ch := make(chan pkg.Result, 1)
	go func() {
		result := r.Execute(code)
		ch <- result
	}()
	return ch
}

// Pump runs pending asyncio events. Called by the dispatcher on every cycle.
// Acquires the GIL since the main thread no longer holds it persistently.
func (r *Runtime) Pump() {
	if !r.initialized {
		return
	}
	gstate := C.PyGILState_Ensure()
	C.PyRun_SimpleString(cPumpCode)
	C.PyGILState_Release(gstate)
}

// Interrupt raises a KeyboardInterrupt in Python at the next bytecode check.
// Writes a byte to the interrupt pipe; a Python daemon thread reads it and
// calls _thread.interrupt_main(). Safe from any goroutine — no GIL needed.
func (r *Runtime) Interrupt() {
	if r.initialized {
		C.omnivm_py_interrupt()
	}
}

// InterruptFuncPtr returns a C function pointer to omnivm_py_interrupt().
// This is safe to call from any thread (including the watchdog pthread)
// because it only performs a write() to the interrupt pipe.
func (r *Runtime) InterruptFuncPtr() unsafe.Pointer {
	return unsafe.Pointer(C.omnivm_py_get_interrupt_ptr())
}

// ClearInterrupt drains any stale interrupt from the pipe and absorbs any
// pending KeyboardInterrupt. Use between tests or after timed interrupts
// where the interrupt goroutine may fire after the target code returns.
func (r *Runtime) ClearInterrupt() {
	if r.initialized {
		C.omnivm_py_clear_interrupt()
	}
}

// Shutdown finalizes CPython.
// In polyglot mode, Py_FinalizeEx can crash when other runtime threads
// (JVM, Ruby proxy) are still active. When running standalone, it can also
// crash due to signal handler teardown conflicts with libjsig.so.
// Since we're exiting the process anyway, skip finalization — same strategy
// as Ruby shutdown.
func (r *Runtime) Shutdown() error {
	if !r.initialized {
		return nil
	}
	r.initialized = false
	// Skip Py_FinalizeEx — process exit reclaims all resources.
	// See Ruby shutdown strategy in MEMORY.md.
	return nil
}

// ActivateForkGuard enables the fork guard (for JVM/Ruby).
// When only Go+JS are loaded, fork is safe if runtimes are initialized post-fork.
func ActivateForkGuard() {
	C.omnivm_activate_fork_guard()
}

// RegisterAppendInittab registers the "omnivm" built-in module with CPython
// via PyImport_AppendInittab. Must be called BEFORE Py_Initialize/Py_BytesMain.
// This is used in Python interpreter mode so "import omnivm" works.
func RegisterAppendInittab() {
	cName := C.CString("omnivm")
	C.PyImport_AppendInittab(cName, (*[0]byte)(C.PyInit_omnivm))
	// Intentionally leak cName — AppendInittab stores the pointer.
}

// BytesMain calls Py_BytesMain with the given arguments, running CPython's
// full CLI (handles -m, -c, script files, interactive REPL, etc.).
// Returns the exit code from CPython.
func BytesMain(args []string) int {
	argc := C.int(len(args))
	argv := make([]*C.char, len(args))
	for i, a := range args {
		argv[i] = C.CString(a)
	}
	// Py_BytesMain takes char** — pass pointer to first element.
	return int(C.Py_BytesMain(argc, &argv[0]))
}

// SetPyModeCallbacks installs the Go callback function pointers used by the
// omnivm Python module in interpreter mode. Called from the main binary.
func SetPyModeCallbacks(initPtr, loadPtr, shutdownPtr, freePtr unsafe.Pointer) {
	C.omnivm_set_pymode_callbacks(
		C.omni_init_runtimes_fn(initPtr),
		C.omni_load_plugin_fn(loadPtr),
		C.omni_shutdown_fn(shutdownPtr),
	)
	// Also set the bridge free function so pymode can free Go-allocated strings
	C.omnivm_py_set_bridge_free(C.omni_free_fn(freePtr))
}

// MarkCPythonInitialized marks CPython as already initialized (because
// Py_BytesMain did it). This prevents double-init if the Go-hosted
// python.Runtime.Initialize() is called later.
func MarkCPythonInitialized() {
	cpythonInitialized = true
}
