package omnivm;

import java.lang.ref.Cleaner;
import java.lang.ref.WeakReference;
import java.util.AbstractMap;
import java.util.ArrayList;
import java.util.Collection;
import java.util.Collections;
import java.util.HashMap;
import java.util.Iterator;
import java.util.LinkedHashSet;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Locale;
import java.util.Map;
import java.util.NoSuchElementException;
import java.util.Set;
import java.util.concurrent.ArrayBlockingQueue;
import java.util.concurrent.BlockingQueue;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.Flow;
import java.util.concurrent.atomic.AtomicBoolean;
import java.util.concurrent.atomic.AtomicLong;
import java.util.concurrent.atomic.AtomicReference;
import java.util.function.Function;
import java.util.stream.Stream;
import java.util.stream.StreamSupport;

/**
 * OmniVM - Cross-runtime bridge for Java.
 *
 * Provides static methods for calling other runtimes (Python, JavaScript, Ruby),
 * sharing typed values via callTyped(), and exchanging binary buffers.
 *
 * Native methods are registered by Go via JNI RegisterNatives during initialization.
 */
public class OmniVM {
    private static final Object PROXY_LIFECYCLE_METHOD_MISSING = new Object();

    /**
     * Call another runtime to evaluate an expression.
     *
     * @param runtime target runtime name ("python", "javascript", "ruby", "java")
     * @param code    expression to evaluate
     * @return result string from the target runtime
     * @throws RuntimeError if the target runtime returns an error
     */
    public static String call(String runtime, String code) {
        try {
            return nativeCall(runtime, code);
        } catch (RuntimeError e) {
            throw e;
        } catch (RuntimeException e) {
            throw RuntimeError.fromBridge(e.getMessage(), runtime, "call[" + runtime + "]", e);
        }
    }

    /**
     * Call a function in another runtime with typed arguments.
     * Returns a typed result (Integer, Long, Double, Boolean, String, byte[], or null).
     *
     * @param runtime  target runtime name
     * @param funcName function name to call
     * @param args     typed arguments (Integer, Long, Double, Boolean, String, byte[])
     * @return typed result from the target runtime
     * @throws RuntimeError if the target runtime returns an error
     */
    public static Object callTyped(String runtime, String funcName, Object... args) {
        try {
            return nativeCallTyped(runtime, funcName, args);
        } catch (RuntimeError e) {
            throw e;
        } catch (RuntimeException e) {
            throw RuntimeError.fromBridge(e.getMessage(), runtime, "call_typed[" + runtime + "]", e);
        }
    }

    /**
     * Get a named buffer from the shared store.
     *
     * @param name buffer name
     * @return buffer data as byte array, or null if not found
     */
    public static byte[] getBuffer(String name) {
        return nativeGetBuffer(name);
    }

    /**
     * Get the dtype tag of a named buffer.
     *
     * @param name buffer name
     * @return dtype integer, or -1 if not found
     */
    public static int getBufferDtype(String name) {
        return nativeGetBufferDtype(name);
    }

    /**
     * Set a named buffer in the shared store.
     *
     * @param name  buffer name
     * @param data  buffer data
     * @param dtype data type tag (0 = raw bytes)
     */
    public static void setBuffer(String name, byte[] data, int dtype) {
        nativeSetBuffer(name, data, dtype);
    }

    /**
     * Set a named buffer with default dtype (0 = raw bytes).
     */
    public static void setBuffer(String name, byte[] data) {
        nativeSetBuffer(name, data, 0);
    }

    /**
     * Release a named buffer from the shared store.
     */
    public static void releaseBuffer(String name) {
        nativeReleaseBuffer(name);
    }

    /**
     * Return JSON lifecycle diagnostics for a named shared buffer.
     */
    public static String bufferStatus(String name) {
        return nativeBufferStatus(name);
    }

    /**
     * Create an AutoCloseable owner for a named buffer that is already published.
     */
    public static BufferOwner bufferOwner(String name) {
        return new BufferOwner(name, null, 0, false).enter();
    }

    /**
     * Scope a named-buffer owner around a callback and release it afterward.
     */
    public static <T> T bufferOwner(String name, Function<BufferOwner, T> body) {
        return withBufferOwner(new BufferOwner(name, null, 0, false).enter(), body);
    }

    /**
     * Publish bytes with dtype 0 and return an AutoCloseable named-buffer owner.
     */
    public static BufferOwner bufferOwner(String name, byte[] data) {
        return new BufferOwner(name, data, 0, true).enter();
    }

    /**
     * Publish bytes with dtype 0, scope the owner around a callback, and release it afterward.
     */
    public static <T> T bufferOwner(String name, byte[] data, Function<BufferOwner, T> body) {
        return withBufferOwner(new BufferOwner(name, data, 0, true).enter(), body);
    }

    /**
     * Publish bytes with an explicit dtype and return an AutoCloseable named-buffer owner.
     */
    public static BufferOwner bufferOwner(String name, byte[] data, int dtype) {
        return new BufferOwner(name, data, dtype, true).enter();
    }

    /**
     * Publish bytes with an explicit dtype, scope the owner around a callback, and release it afterward.
     */
    public static <T> T bufferOwner(String name, byte[] data, int dtype, Function<BufferOwner, T> body) {
        return withBufferOwner(new BufferOwner(name, data, dtype, true).enter(), body);
    }

    private static <T> T withBufferOwner(BufferOwner owner, Function<BufferOwner, T> body) {
        try {
            T result = body.apply(owner);
            owner.release();
            return result;
        } catch (RuntimeException | Error err) {
            try {
                owner.release();
            } catch (RuntimeException | Error cleanupError) {
                err.addSuppressed(cleanupError);
            }
            throw err;
        }
    }

    /**
     * Return the owner-dispatch capability contract for this embedded Java runtime.
     */
    @SuppressWarnings("unchecked")
    public static Map<String, Object> ownerDispatchStatus() {
        return (Map<String, Object>) RuntimeError.copyJsonValue(ownerDispatchContract());
    }

    /**
     * Return the embedded Ruby threading capability contract.
     */
    @SuppressWarnings("unchecked")
    public static Map<String, Object> rubyThreadingStatus() {
        return (Map<String, Object>) RuntimeError.copyJsonValue(rubyThreadingContract());
    }

    /**
     * Return one normalized owner-dispatch target capability block.
     */
    @SuppressWarnings("unchecked")
    public static Map<String, Object> ownerDispatchTargetStatus(String target) {
        String requested = String.valueOf(target);
        String name = ownerDispatchTargetName(requested);
        Map<String, Object> status = ownerDispatchStatus();
        Object targets = status.get("owner_dispatch_targets");
        Map<?, ?> targetMap = targets instanceof Map<?, ?> map ? map : null;
        if (targetMap == null || !targetMap.containsKey(name)) {
            List<Object> knownTargets = ownerDispatchList();
            if (targetMap != null) {
                for (Object key : targetMap.keySet()) {
                    knownTargets.add(String.valueOf(key));
                }
                knownTargets.sort((left, right) -> String.valueOf(left).compareTo(String.valueOf(right)));
            }
            throw runtimeError(
                "unknown owner dispatch target: " + requested,
                "owner_dispatch_target",
                ownerDispatchMap(
                    "target", name,
                    "requested_target", requested,
                    "known_targets", knownTargets,
                    "owner_dispatch_targets", targets,
                    "owner_dispatch_target", ownerDispatchMap(
                        "target", name,
                        "requested_target", requested,
                        "known_targets", knownTargets,
                        "owner_dispatch_targets", targets)));
        }
        Map<String, Object> info = (Map<String, Object>) RuntimeError.copyJsonValue(targetMap.get(name));
        info.put("requested_target", requested);
        info.put("target", name);
        return info;
    }

    public static boolean assertOwnerDispatchSupported() {
        return assertOwnerDispatchSupported("");
    }

    public static boolean assertOwnerDispatchSupported(String label) {
        Map<String, Object> info = ownerDispatchStatus();
        if (Boolean.TRUE.equals(info.get("owner_dispatch_supported"))) {
            return true;
        }
        String prefix = label == null || label.isEmpty() ? "" : label + ": ";
        throw runtimeError(
            prefix + "owner dispatch unsupported: " + String.valueOf(info.get("reason")),
            "owner_dispatch",
            ownerDispatchMap("owner_dispatch", info));
    }

    public static boolean assertRubyNativeThreadsSupported() {
        return assertRubyNativeThreadsSupported("");
    }

    public static boolean assertRubyNativeThreadsSupported(String label) {
        Map<String, Object> info = rubyThreadingStatus();
        if (Boolean.TRUE.equals(info.get("native_threads_supported"))) {
            return true;
        }
        String prefix = label == null || label.isEmpty() ? "" : label + ": ";
        throw runtimeError(
            prefix + "native Ruby threads unsupported: mode=" + String.valueOf(info.get("mode")) + ": " + String.valueOf(info.get("diagnostic")),
            "ruby_threading",
            ownerDispatchMap("ruby_threading", info));
    }

    public static boolean assertOwnerDispatchTargetSupported(String target) {
        return assertOwnerDispatchTargetSupported(target, "");
    }

    public static boolean assertOwnerDispatchTargetSupported(String target, String label) {
        Map<String, Object> info = ownerDispatchTargetStatus(target);
        if (Boolean.TRUE.equals(info.get("supported"))) {
            return true;
        }
        String prefix = label == null || label.isEmpty() ? "" : label + ": ";
        throw runtimeError(
            prefix + "owner dispatch target unsupported: " + String.valueOf(info.get("target")) + ": " + String.valueOf(info.get("diagnostic")),
            "owner_dispatch_target",
            ownerDispatchMap(
                "target", info.get("target"),
                "requested_target", info.get("requested_target"),
                "owner_dispatch_target", info));
    }

    private static RuntimeError runtimeError(String message, String boundaryPath, Object details) {
        ParsedRuntimeError parsed = new ParsedRuntimeError();
        parsed.runtime = "java";
        parsed.originRuntime = "java";
        parsed.type = "RuntimeError";
        parsed.message = message;
        parsed.boundaryPath = boundaryPath;
        parsed.detailsJson = jsonValue(RuntimeError.copyJsonValue(details));
        return new RuntimeError(parsed, null);
    }

    private static Map<String, Object> ownerDispatchContract() {
        Map<String, Object> targets = ownerDispatchMap(
            "python_asyncio", ownerDispatchMap(
                "supported", false,
                "owner_kind", "python_asyncio_loop",
                "required_capability", "run callback on owning asyncio loop",
                "current_behavior", "Python async stream pulls and close have narrow pump-owned paths; general callbacks are not migrated back to the owner loop",
                "diagnostic", "Python async streams have narrow pump-owned pull/close paths, but general callbacks are not migrated back to the owner loop",
                "narrow_capabilities", ownerDispatchList("python_async_stream_pull", "python_async_stream_close")),
            "javascript_event_loop", ownerDispatchMap(
                "supported", false,
                "owner_kind", "javascript_event_loop",
                "required_capability", "run callback on the owning JavaScript event loop",
                "current_behavior", "JavaScript promises and timers are pumped at OmniVM call boundaries; foreign owner-loop callback dispatch is not available",
                "diagnostic", "OmniVM does not currently route arbitrary callbacks back onto a JavaScript event loop owner"),
            "java_executor", ownerDispatchMap(
                "supported", false,
                "owner_kind", "java_executor",
                "required_capability", "run callback on the owning Java Executor",
                "current_behavior", "Java futures and reactive handles expose cancellation/status, but arbitrary callbacks are not migrated to a captured Executor",
                "diagnostic", "OmniVM does not currently route arbitrary callbacks back onto a Java Executor owner"),
            "ruby_fiber_thread", ownerDispatchMap(
                "supported", false,
                "owner_kind", "ruby_fiber_thread",
                "required_capability", "run callback on the owning Ruby Fiber or native Thread",
                "current_behavior", "Ruby runs on the single VM thread with native Ruby thread scheduling disabled",
                "diagnostic", "Ruby runs on the single VM thread; native Ruby thread scheduling and Puma-style in-process thread ownership remain unsupported"));
        return ownerDispatchMap(
            "mode", "diagnostic_only",
            "host_thread_required", true,
            "owner_dispatch_supported", false,
            "foreign_thread_behavior", "reject_runtime_calls",
            "reason", "owner dispatch is unsupported in this mode, so OmniVM will not route calls onto foreign owner loops",
            "owner_dispatch_targets", targets);
    }

    private static Map<String, Object> rubyThreadingContract() {
        return ownerDispatchMap(
            "mode", "single_vm_thread",
            "native_threads_supported", false,
            "ruby_vm_thread", "single_vm_thread",
            "thread_new_behavior", "unsupported_diagnostic",
            "diagnostic", "Ruby runs on the single VM thread; native Ruby thread scheduling and Puma-style in-process thread ownership remain unsupported",
            "app_server_boundary", "Use Fiber/Async or single-thread Rack servers in process; run native-threaded Ruby app servers such as Puma out of process.");
    }

    private static String ownerDispatchTargetName(String target) {
        String normalized = String.valueOf(target).trim().toLowerCase(Locale.ROOT).replace('-', '_').replace(' ', '_');
        return switch (normalized) {
            case "asyncio", "python", "python_loop", "python_async_loop", "py" -> "python_asyncio";
            case "js", "javascript", "javascript_loop", "node", "nodejs", "event_loop" -> "javascript_event_loop";
            case "java", "jvm", "executor" -> "java_executor";
            case "ruby", "fiber", "thread", "ruby_fiber", "ruby_thread" -> "ruby_fiber_thread";
            default -> normalized;
        };
    }

    private static Map<String, Object> ownerDispatchMap(Object... pairs) {
        Map<String, Object> out = new LinkedHashMap<>();
        for (int i = 0; i + 1 < pairs.length; i += 2) {
            out.put(String.valueOf(pairs[i]), pairs[i + 1]);
        }
        return out;
    }

    private static List<Object> ownerDispatchList(Object... values) {
        List<Object> out = new ArrayList<>();
        for (Object value : values) {
            out.add(value);
        }
        return out;
    }

    private static boolean runtimeErrorBufferWasReleased(RuntimeError err) {
        Object details = err.getDetails();
        if (bufferStatusIsReleased(details)) {
            return true;
        }
        if (details instanceof Map<?, ?> map) {
            return bufferStatusIsReleased(map.get("buffer"));
        }
        return false;
    }

    private static boolean bufferStatusIsReleased(Object status) {
        if (!(status instanceof Map<?, ?> map)) {
            return false;
        }
        if (Boolean.TRUE.equals(map.get("released"))) {
            return true;
        }
        Object state = map.get("state");
        return "released".equals(String.valueOf(state)) || "released_detached".equals(String.valueOf(state));
    }

    public static final class BufferOwner implements AutoCloseable {
        private final String name;
        private final byte[] data;
        private final int dtype;
        private final boolean hasData;
        private boolean entered;
        private boolean released;

        private BufferOwner(String name, byte[] data, int dtype, boolean hasData) {
            this.name = String.valueOf(name);
            this.data = data;
            this.dtype = dtype;
            this.hasData = hasData;
        }

        public String name() {
            return name;
        }

        public boolean isReleased() {
            return released;
        }

        public String status() {
            return bufferStatus(name);
        }

        public BufferOwner enter() {
            if (released) {
                throw runtimeError(
                    "OmniVM.bufferOwner \"" + name + "\" cannot be re-entered after release",
                    "native_memory",
                    ownerDispatchMap("buffer", ownerDispatchMap("name", name, "released", true)));
            }
            if (entered) {
                throw runtimeError(
                    "OmniVM.bufferOwner \"" + name + "\" is already active",
                    "native_memory",
                    ownerDispatchMap("buffer", ownerDispatchMap("name", name, "active_owner", true)));
            }
            if (hasData) {
                if (data == null) {
                    throw new IllegalArgumentException("bufferOwner data cannot be null");
                }
                setBuffer(name, data, dtype);
            }
            entered = true;
            return this;
        }

        public boolean release() {
            if (released) {
                return false;
            }
            try {
                releaseBuffer(name);
            } catch (RuntimeError err) {
                if (runtimeErrorBufferWasReleased(err)) {
                    released = true;
                    entered = false;
                }
                throw err;
            }
            released = true;
            entered = false;
            return true;
        }

        @Override
        public void close() {
            release();
        }
    }

    // Native bridge methods — implemented in Go via JNI RegisterNatives.
    public static native String nativeCall(String runtime, String code);
    public static native byte[] nativeGetBuffer(String name);
    public static native int nativeGetBufferDtype(String name);
    public static native void nativeSetBuffer(String name, byte[] data, int dtype);
    public static native void nativeReleaseBuffer(String name);
    public static native String nativeBufferStatus(String name);
    public static native Object nativeCallTyped(String runtime, String funcName, Object[] args);

    public static class RuntimeError extends RuntimeException {
        private final String runtime;
        private final String originRuntime;
        private final String type;
        private final String traceback;
        private final List<String> stackFrames;
        private final List<Map<String, Object>> causeChain;
        private final String boundaryPath;
        private final String originalErrorHandle;
        private final Object details;
        private final String detailsJson;

        private RuntimeError(ParsedRuntimeError parsed, Throwable cause) {
            super(parsed.message, cause);
            this.runtime = parsed.runtime;
            this.originRuntime = parsed.originRuntime == null || parsed.originRuntime.isEmpty() ? parsed.runtime : parsed.originRuntime;
            this.type = parsed.type;
            this.traceback = parsed.traceback;
            this.stackFrames = Collections.unmodifiableList(parsed.stackFrames);
            this.causeChain = Collections.unmodifiableList(parsed.causeChain);
            this.boundaryPath = parsed.boundaryPath;
            this.originalErrorHandle = parsed.originalErrorHandle;
            this.details = copyJsonValue(parseDetailsJson(parsed.detailsJson));
            this.detailsJson = parsed.detailsJson;
        }

        public String getRuntime() {
            return runtime;
        }

        public String getOriginRuntime() {
            return originRuntime;
        }

        public String getType() {
            return type;
        }

        public String getTraceback() {
            return traceback;
        }

        public List<String> getStackFrames() {
            return stackFrames;
        }

        public List<Map<String, Object>> getCauseChain() {
            return copyCauseChain(causeChain);
        }

        public String getBoundaryPath() {
            return boundaryPath;
        }

        public String getOriginalErrorHandle() {
            return originalErrorHandle;
        }

        public Object getDetails() {
            return copyJsonValue(details);
        }

        public String getDetailsJson() {
            return detailsJson;
        }

        public Map<String, Object> toMap() {
            Map<String, Object> out = new LinkedHashMap<>();
            out.put("runtime", runtime);
            out.put("origin_runtime", originRuntime);
            out.put("type", type);
            out.put("message", getMessage());
            out.put("traceback", traceback);
            out.put("stack_frames", new ArrayList<>(stackFrames));
            out.put("cause_chain", copyJsonValue(causeChain));
            out.put("boundary_path", boundaryPath);
            out.put("original_error_handle", originalErrorHandle);
            out.put("details", copyJsonValue(details));
            out.put("details_json", detailsJson);
            return out;
        }

        public String toJson() {
            return jsonValue(toMap());
        }

        static RuntimeError fromBridge(String bridgeMessage, String fallbackRuntime, String fallbackBoundary, Throwable cause) {
            return new RuntimeError(parseBridgeRuntimeError(bridgeMessage, fallbackRuntime, fallbackBoundary), cause);
        }

        private static Object parseDetailsJson(String detailsJson) {
            if (detailsJson == null || detailsJson.isEmpty()) {
                return null;
            }
            try {
                return parseJson(detailsJson);
            } catch (RuntimeException ignored) {
                return detailsJson;
            }
        }

        private static Object copyJsonValue(Object value) {
            if (value instanceof Map<?, ?> map) {
                Map<String, Object> out = new LinkedHashMap<>();
                for (Map.Entry<?, ?> entry : map.entrySet()) {
                    out.put(String.valueOf(entry.getKey()), copyJsonValue(entry.getValue()));
                }
                return out;
            }
            if (value instanceof Iterable<?> iterable && !(value instanceof CharSequence)) {
                List<Object> out = new ArrayList<>();
                for (Object item : iterable) {
                    out.add(copyJsonValue(item));
                }
                return out;
            }
            return value;
        }

        @SuppressWarnings("unchecked")
        private static List<Map<String, Object>> copyCauseChain(List<Map<String, Object>> causes) {
            return (List<Map<String, Object>>) copyJsonValue(causes);
        }
    }

    private static class ParsedRuntimeError {
        String runtime = "";
        String originRuntime = "";
        String type = "";
        String message = "";
        String traceback;
        List<String> stackFrames = new ArrayList<>();
        List<Map<String, Object>> causeChain = new ArrayList<>();
        String boundaryPath = "";
        String originalErrorHandle;
        String detailsJson;
    }

    private static ParsedRuntimeError parseBridgeRuntimeError(String bridgeMessage, String fallbackRuntime, String fallbackBoundary) {
        ParsedRuntimeError parsed = new ParsedRuntimeError();
        parsed.runtime = safeString(fallbackRuntime);
        parsed.boundaryPath = safeString(fallbackBoundary);

        String text = safeString(bridgeMessage).trim();
        if (text.startsWith("ERR:")) {
            text = text.substring(4).trim();
        }
        ParsedRuntimeError envelope = parseStructuredErrorEnvelope(text, parsed.runtime, parsed.boundaryPath);
        if (envelope != null) {
            return envelope;
        }
        parsed.originalErrorHandle = extractOriginalErrorHandle(text);
        parsed.detailsJson = extractDetailsJson(text);

        List<String> boundaryParts = new ArrayList<>();
        text = stripBoundaryPrefix(text, "execute manifest", boundaryParts);
        text = stripBoundaryPrefix(text, "load manifest module", boundaryParts);
        text = stripBoundaryPrefix(text, "manifest module call", boundaryParts);
        text = stripCallBoundary(text, parsed, boundaryParts);
        text = stripRuntimeRefAssignPrefix(text, parsed);
        text = stripRuntimePrefixes(text, parsed);

        String wrappedBoundary = parsed.boundaryPath;
        if (!boundaryParts.isEmpty()) {
            wrappedBoundary = String.join(" > ", boundaryParts);
        } else if (!parsed.runtime.isEmpty() && !parsed.runtime.equals(safeString(fallbackRuntime))) {
            wrappedBoundary = "call[" + parsed.runtime + "]";
        } else if (wrappedBoundary.isEmpty() && !parsed.runtime.isEmpty()) {
            wrappedBoundary = "call[" + parsed.runtime + "]";
        }

        envelope = parseStructuredErrorEnvelope(text, parsed.runtime, wrappedBoundary);
        if (envelope != null) {
            return envelope;
        }

        if (!boundaryParts.isEmpty()) {
            parsed.boundaryPath = wrappedBoundary;
        } else if (!parsed.runtime.isEmpty() && !parsed.runtime.equals(safeString(fallbackRuntime))) {
            parsed.boundaryPath = wrappedBoundary;
        } else if (parsed.boundaryPath.isEmpty() && !parsed.runtime.isEmpty()) {
            parsed.boundaryPath = wrappedBoundary;
        }

        parseMessageAndType(text, parsed);
        parsed.originRuntime = parsed.runtime;
        parsed.stackFrames = parseStackFrames(parsed.traceback);
        parsed.causeChain = parseCauseChain(text, parsed.runtime);
        if (parsed.message.isEmpty()) {
            parsed.message = text;
        }
        return parsed;
    }

    @SuppressWarnings("unchecked")
    private static ParsedRuntimeError parseStructuredErrorEnvelope(String text, String fallbackRuntime, String fallbackBoundary) {
        String body = safeString(text).trim();
        if (!body.startsWith("{")) {
            return null;
        }
        Object parsedJson;
        try {
            parsedJson = parseJson(body);
        } catch (RuntimeException ignored) {
            return null;
        }
        if (!(parsedJson instanceof Map<?, ?> rawEnvelope)) {
            return null;
        }
        Map<String, Object> envelope = new LinkedHashMap<>();
        for (Map.Entry<?, ?> entry : rawEnvelope.entrySet()) {
            envelope.put(String.valueOf(entry.getKey()), entry.getValue());
        }
        ParsedRuntimeError parsed = new ParsedRuntimeError();
        parsed.runtime = nonEmptyJsonString(envelope.get("runtime"), safeString(fallbackRuntime));
        parsed.originRuntime = nonEmptyJsonString(jsonValue(envelope, "origin_runtime", "originRuntime"), parsed.runtime);
        parsed.type = jsonString(jsonValue(envelope, "type", "name", "error_type", "errorType"));
        parsed.message = jsonString(envelope.get("message"));
        parsed.traceback = jsonString(jsonValue(envelope, "traceback", "stack"));
        if (parsed.runtime.isEmpty() && parsed.type.isEmpty() && parsed.message.isEmpty() && safeString(parsed.traceback).isEmpty()) {
            return null;
        }
        parsed.boundaryPath = nonEmptyJsonString(jsonValue(envelope, "boundary_path", "boundaryPath"), safeString(fallbackBoundary));
        parsed.originalErrorHandle = emptyToNull(jsonString(jsonValue(envelope, "original_error_handle", "originalErrorHandle")));
        parsed.detailsJson = detailsJsonValue(envelope);
        parsed.stackFrames = stringListJsonValue(jsonValue(envelope, "stack_frames", "stackFrames"), parseStackFrames(parsed.traceback));
        parsed.causeChain = causeChainJsonValue(jsonValue(envelope, "cause_chain", "causeChain"), parsed.runtime);
        return parsed;
    }

    private static String detailsJsonValue(Map<?, ?> envelope) {
        if (envelope.containsKey("details")) {
            return jsonValue(RuntimeError.copyJsonValue(envelope.get("details")));
        }
        Object rawDetails = jsonValue(envelope, "details_json", "detailsJson");
        if (rawDetails instanceof String text) {
            return text;
        }
        if (rawDetails != null) {
            return jsonValue(RuntimeError.copyJsonValue(rawDetails));
        }
        return null;
    }

    private static Object detailsObjectValue(Map<?, ?> source) {
        if (source.containsKey("details")) {
            return RuntimeError.copyJsonValue(source.get("details"));
        }
        Object rawDetails = jsonValue(source, "details_json", "detailsJson");
        if (rawDetails instanceof String text) {
            return RuntimeError.parseDetailsJson(text);
        }
        return rawDetails == null ? null : RuntimeError.copyJsonValue(rawDetails);
    }

    private static Object jsonValue(Map<?, ?> value, String... keys) {
        for (String key : keys) {
            Object item = value.get(key);
            if (item != null) {
                return item;
            }
        }
        return null;
    }

    private static String jsonString(Object value) {
        return value == null ? "" : String.valueOf(value);
    }

    private static String nonEmptyJsonString(Object value, String fallback) {
        String text = jsonString(value);
        return text.isEmpty() ? safeString(fallback) : text;
    }

    private static String emptyToNull(String value) {
        return value == null || value.isEmpty() ? null : value;
    }

    private static List<String> stringListJsonValue(Object value, List<String> fallback) {
        if (!(value instanceof Iterable<?> iterable)) {
            return fallback;
        }
        List<String> out = new ArrayList<>();
        for (Object item : iterable) {
            if (!(item instanceof String text)) {
                return fallback;
            }
            out.add(text);
        }
        return out;
    }

    private static List<Map<String, Object>> causeChainJsonValue(Object value, String fallbackRuntime) {
        List<Map<String, Object>> out = new ArrayList<>();
        if (!(value instanceof Iterable<?> iterable)) {
            return out;
        }
        String defaultRuntime = safeString(fallbackRuntime);
        for (Object item : iterable) {
            if (!(item instanceof Map<?, ?> cause)) {
                continue;
            }
            Map<String, Object> entry = new LinkedHashMap<>();
            entry.put("type", jsonString(jsonValue(cause, "type", "name", "error_type", "errorType")));
            entry.put("message", jsonString(cause.get("message")));
            String traceback = jsonString(jsonValue(cause, "traceback", "stack"));
            if (!traceback.isEmpty()) {
                entry.put("traceback", traceback);
            }
            List<String> stackFrames = stringListJsonValue(jsonValue(cause, "stack_frames", "stackFrames"), parseStackFrames(traceback));
            if (!stackFrames.isEmpty()) {
                entry.put("stack_frames", stackFrames);
            }
            String runtime = nonEmptyJsonString(cause.get("runtime"), defaultRuntime);
            if (!runtime.isEmpty()) {
                entry.put("runtime", runtime);
            }
            String originRuntime = jsonString(jsonValue(cause, "origin_runtime", "originRuntime"));
            if (originRuntime.isEmpty()) {
                originRuntime = runtime;
            }
            if (!originRuntime.isEmpty()) {
                entry.put("origin_runtime", originRuntime);
            }
            String boundaryPath = jsonString(jsonValue(cause, "boundary_path", "boundaryPath"));
            if (!boundaryPath.isEmpty()) {
                entry.put("boundary_path", boundaryPath);
            }
            String originalErrorHandle = jsonString(jsonValue(cause, "original_error_handle", "originalErrorHandle"));
            if (!originalErrorHandle.isEmpty()) {
                entry.put("original_error_handle", originalErrorHandle);
            }
            Object causeDetails = detailsObjectValue(cause);
            if (cause.containsKey("details") || causeDetails != null) {
                entry.put("details", causeDetails);
            }
            String causeDetailsJson = detailsJsonValue(cause);
            if (causeDetailsJson != null) {
                entry.put("details_json", causeDetailsJson);
            }
            out.add(Collections.unmodifiableMap(entry));
        }
        return out;
    }

    private static String stripBoundaryPrefix(String text, String prefix, List<String> boundaryParts) {
        String marker = prefix + ":";
        if (text.startsWith(marker)) {
            boundaryParts.add(prefix);
            return text.substring(marker.length()).trim();
        }
        return text;
    }

    private static String stripCallBoundary(String text, ParsedRuntimeError parsed, List<String> boundaryParts) {
        int colon = text.indexOf(": ");
        if (colon <= 0) {
            return text;
        }
        String head = text.substring(0, colon);
        int open = head.indexOf('[');
        int close = head.indexOf(']', open + 1);
        if (open <= 0 || close <= open || close != head.length() - 1) {
            return text;
        }
        String op = head.substring(0, open);
        String runtime = head.substring(open + 1, close);
        if (!isIdentifierLike(op) || !isRuntimeLike(runtime)) {
            return text;
        }
        parsed.runtime = normalizeRuntime(runtime);
        boundaryParts.add(op + "[" + runtime + "]");
        return text.substring(colon + 2).trim();
    }

    private static String stripRuntimePrefixes(String text, ParsedRuntimeError parsed) {
        boolean changed = true;
        while (changed) {
            changed = false;
            int colon = text.indexOf(": ");
            if (colon <= 0) {
                continue;
            }
            String prefix = text.substring(0, colon).trim();
            if (isRuntimeLike(prefix)) {
                parsed.runtime = normalizeRuntime(prefix);
                text = text.substring(colon + 2).trim();
                changed = true;
            }
        }
        return text;
    }

    private static String stripRuntimeRefAssignPrefix(String text, ParsedRuntimeError parsed) {
        String prefix = "runtime ref assign [";
        if (!text.startsWith(prefix)) {
            return text;
        }
        int close = text.indexOf("]: ", prefix.length());
        if (close < 0) {
            return text;
        }
        String runtime = text.substring(prefix.length(), close);
        if (!isRuntimeLike(runtime)) {
            return text;
        }
        parsed.runtime = normalizeRuntime(runtime);
        return text.substring(close + 3).trim();
    }

    private static void parseMessageAndType(String text, ParsedRuntimeError parsed) {
        String[] lines = text.split("\\R", -1);
        String firstLine = lines.length == 0 ? text : lines[0].trim();
        if (firstLine.startsWith("Traceback ")) {
            parsed.traceback = text;
            for (int i = lines.length - 1; i >= 0; i--) {
                String line = lines[i].trim();
                if (line.isEmpty() || isMetadataLine(line)) {
                    continue;
                }
                int colon = line.indexOf(": ");
                if (colon > 0 && isErrorTypeCandidate(line.substring(0, colon).trim())) {
                    parseTypeLine(line, parsed);
                    return;
                }
            }
            parsed.message = firstLine;
            return;
        }

        parseTypeLine(firstLine, parsed);
        if (lines.length > 1) {
            String rest = text.substring(lines[0].length()).trim();
            if (!rest.isEmpty()) {
                parsed.traceback = rest;
            }
        }
    }

    private static void parseTypeLine(String line, ParsedRuntimeError parsed) {
        int colon = line.indexOf(": ");
        if (colon > 0) {
            String candidate = line.substring(0, colon).trim();
            if (isErrorTypeCandidate(candidate)) {
                parsed.type = simpleTypeName(candidate, parsed.runtime);
                parsed.message = line.substring(colon + 2).trim();
                return;
            }
        }
        parsed.message = line.trim();
    }

    private static boolean isOriginalErrorHandleLine(String line) {
        String lower = safeString(line).toLowerCase(Locale.ROOT);
        return lower.startsWith("original error handle:")
            || lower.startsWith("original-error-handle:")
            || lower.startsWith("original_error_handle:");
    }

    private static boolean isMetadataLine(String line) {
        String lower = safeString(line).toLowerCase(Locale.ROOT);
        return lower.startsWith("caused by:")
            || lower.startsWith("details:")
            || lower.startsWith("details_json:")
            || lower.startsWith("detailsjson:")
            || isOriginalErrorHandleLine(line);
    }

    private static String detailsMetadataLabel(String line) {
        String text = safeString(line).trim();
        int colon = text.indexOf(':');
        if (colon < 0) {
            return "";
        }
        String label = text.substring(0, colon).trim().toLowerCase(Locale.ROOT);
        return "details".equals(label) || "details_json".equals(label) || "detailsjson".equals(label) ? label : "";
    }

    private static List<Map<String, Object>> parseCauseChain(String text, String fallbackRuntime) {
        List<Map<String, Object>> causes = new ArrayList<>();
        String runtime = safeString(fallbackRuntime);
        String[] lines = text.split("\\R");
        for (String rawLine : lines) {
            String line = rawLine.trim();
            if (!line.startsWith("Caused by: ")) {
                continue;
            }
            String detail = line.substring("Caused by: ".length()).trim();
            String type = "";
            String message = detail;
            int colon = detail.indexOf(": ");
            if (colon > 0) {
                String candidate = detail.substring(0, colon).trim();
                if (isErrorTypeCandidate(candidate)) {
                    type = simpleTypeName(candidate, "");
                    message = detail.substring(colon + 2).trim();
                }
            }
            Map<String, Object> entry = new LinkedHashMap<>();
            entry.put("type", type);
            entry.put("message", message);
            if (!runtime.isEmpty()) {
                entry.put("runtime", runtime);
                entry.put("origin_runtime", runtime);
            }
            causes.add(Collections.unmodifiableMap(entry));
        }
        return causes;
    }

    private static List<String> parseStackFrames(String traceback) {
        List<String> frames = new ArrayList<>();
        String[] lines = safeString(traceback).split("\\R");
        for (String rawLine : lines) {
            String line = rawLine.trim();
            if (line.isEmpty() || isMetadataLine(line)) {
                continue;
            }
            frames.add(line);
        }
        return frames;
    }

    private static String extractOriginalErrorHandle(String text) {
        String[] lines = text.split("\\R");
        for (String rawLine : lines) {
            String line = rawLine.trim();
            String lower = line.toLowerCase();
            if (lower.startsWith("original_error_handle:") || lower.startsWith("original error handle:") || lower.startsWith("original-error-handle:")) {
                int colon = line.indexOf(':');
                String handle = line.substring(colon + 1).trim();
                return handle.isEmpty() ? null : handle;
            }
        }
        return null;
    }

    private static String extractDetailsJson(String text) {
        String[] lines = text.split("\\R");
        for (String rawLine : lines) {
            String line = rawLine.trim();
            String label = detailsMetadataLabel(line);
            if (!label.isEmpty()) {
                String details = line.substring(line.indexOf(':') + 1).trim();
                return details.isEmpty() ? null : details;
            }
        }
        return null;
    }

    private static boolean isRuntimeLike(String value) {
        String normalized = normalizeRuntime(value);
        return "python".equals(normalized) || "javascript".equals(normalized) || "ruby".equals(normalized)
            || "java".equals(normalized) || "go".equals(normalized) || "__manifest".equals(normalized);
    }

    private static String normalizeRuntime(String value) {
        String runtime = safeString(value).trim().toLowerCase();
        if ("js".equals(runtime) || "node".equals(runtime)) {
            return "javascript";
        }
        if ("jvm".equals(runtime)) {
            return "java";
        }
        return runtime;
    }

    private static boolean isIdentifierLike(String value) {
        if (value.isEmpty()) {
            return false;
        }
        char first = value.charAt(0);
        if (!Character.isLetter(first) && first != '_') {
            return false;
        }
        for (int i = 1; i < value.length(); i++) {
            char c = value.charAt(i);
            if (!Character.isLetterOrDigit(c) && c != '_') {
                return false;
            }
        }
        return true;
    }

    private static String simpleTypeName(String typeName, String runtime) {
        String safe = safeString(typeName);
        if ("python".equals(normalizeRuntime(runtime))) {
            int dot = safe.lastIndexOf('.');
            if (dot >= 0 && dot + 1 < safe.length()) {
                return safe.substring(dot + 1);
            }
        }
        return safe;
    }

    private static boolean isErrorTypeCandidate(String value) {
        if (value == null || value.isEmpty()) {
            return false;
        }
        char first = value.charAt(0);
        if (!Character.isLetter(first) && first != '_') {
            return false;
        }
        for (int i = 0; i < value.length(); i++) {
            char c = value.charAt(i);
            if (!Character.isLetterOrDigit(c) && c != '_' && c != '.' && c != '$' && c != ':') {
                return false;
            }
        }
        return true;
    }

    private static String safeString(String value) {
        return value == null ? "" : value;
    }

    // Capture storage for manifest executor.
    private static final Map<String, Object> captures = new HashMap<>();
    private static final Map<String, String> captureJson = new HashMap<>();
    private static final Map<String, Object> argRefs = new ConcurrentHashMap<>();
    private static final AtomicLong argRefCounter = new AtomicLong();
    private static final Cleaner captureCleaner = Cleaner.create();
    private static final Map<String, WeakReference<Object>> proxyCache = new ConcurrentHashMap<>();
    private static final int proxyCachePruneThreshold = 4096;

    /**
     * Set a capture value (called by manifest executor before Java code runs).
     */
    public static void setCapture(String name, String jsonValue) {
        captureJson.put(name, jsonValue);
        captures.put(name, materializeCapture(parseJson(jsonValue)));
    }

    /**
     * Get a capture value from the manifest executor.
     */
    public static Object getCapture(String name) {
        return captures.get(name);
    }

    /**
     * Store a live Java object in the manifest capture table.
     */
    public static void setCaptureObject(String name, Object value) {
        captureJson.remove(name);
        captures.put(name, value);
    }

    /**
     * Get the raw JSON representation of a capture for debugging or explicit parsing.
     */
    public static String getCaptureJson(String name) {
        return captureJson.get(name);
    }

    /**
     * Clear all captures.
     */
    public static void clearCaptures() {
        captures.clear();
        captureJson.clear();
    }

    /**
     * Clear one manifest-injected capture without disturbing persistent Java bindings.
     */
    public static void clearCapture(String name) {
        captures.remove(name);
        captureJson.remove(name);
    }

    public static Object fromJson(String json) {
        return parseJson(json);
    }

    public static Object materializeJsonCapture(String json) {
        return materializeCapture(parseJson(json));
    }

    public static Object getArgRef(String id) {
        return argRefs.get(id);
    }

    public static void releaseArgRef(String id) {
        if (id != null) {
            argRefs.remove(id);
        }
    }

    public static List<Object> listOf(Object[] values) {
        List<Object> out = new ArrayList<>();
        if (values != null) {
            Collections.addAll(out, values);
        }
        return out;
    }

    public static Map<String, Object> mapOf(Object[] pairs) {
        Map<String, Object> out = new LinkedHashMap<>();
        if (pairs == null) {
            return out;
        }
        for (int i = 0; i+1 < pairs.length; i += 2) {
            out.put(String.valueOf(pairs[i]), pairs[i+1]);
        }
        return out;
    }

    public static Object kwargsRecord(String targetType, Object kwargsValue, Object keysValue) {
        try {
            Class<?> type = Class.forName(targetType);
            Map<String, Object> kwargs = coerceStringMap(kwargsValue);
            List<String> keys = coerceStringList(keysValue);
            if (type.isRecord()) {
                java.lang.reflect.RecordComponent[] components = type.getRecordComponents();
                Object[] args = new Object[components.length];
                Class<?>[] argTypes = new Class<?>[components.length];
                for (int i = 0; i < components.length; i++) {
                    String name = components[i].getName();
                    args[i] = coerceArg(kwargs.get(name), components[i].getType());
                    argTypes[i] = components[i].getType();
                }
                java.lang.reflect.Constructor<?> ctor = type.getDeclaredConstructor(argTypes);
                makeAccessible(ctor);
                return ctor.newInstance(args);
            }
            if (keys.isEmpty()) {
                keys.addAll(kwargs.keySet());
            }
            for (java.lang.reflect.Constructor<?> ctor : type.getDeclaredConstructors()) {
                if (ctor.getParameterCount() != keys.size()) {
                    continue;
                }
                Object[] args = new Object[keys.size()];
                Class<?>[] argTypes = ctor.getParameterTypes();
                for (int i = 0; i < keys.size(); i++) {
                    args[i] = coerceArg(kwargs.get(keys.get(i)), argTypes[i]);
                }
                makeAccessible(ctor);
                return ctor.newInstance(args);
            }
        } catch (ReflectiveOperationException | RuntimeException ignored) {
        }
        return null;
    }

    public static Object kwargsBuilder(String targetType, Object kwargsValue, Object keysValue) {
        try {
            Class<?> type = Class.forName(targetType);
            Object builder = type.getDeclaredConstructor().newInstance();
            Map<String, Object> kwargs = coerceStringMap(kwargsValue);
            List<String> keys = coerceStringList(keysValue);
            if (keys.isEmpty()) {
                keys.addAll(kwargs.keySet());
            }
            for (String key : keys) {
                java.lang.reflect.Method setter = builderSetter(type, key);
                if (setter == null) {
                    return null;
                }
                makeAccessible(setter);
                setter.invoke(builder, coerceArg(kwargs.get(key), setter.getParameterTypes()[0]));
            }
            java.lang.reflect.Method build = zeroArgMethod(type, "build");
            if (build != null) {
                makeAccessible(build);
                return build.invoke(builder);
            }
            return builder;
        } catch (ReflectiveOperationException | RuntimeException ignored) {
            return null;
        }
    }

    public static String toJson(Object value) {
        return jsonBridgeValue(value);
    }

    public static String primitiveSnapshot(Object value) {
        Map<String, Object> snapshot = new LinkedHashMap<>();
        if (value == null
            || value instanceof Number
            || value instanceof Boolean
            || value instanceof String
            || value instanceof Character) {
            snapshot.put("primitive", true);
            snapshot.put("value", value == null ? null : value);
        } else {
            snapshot.put("primitive", false);
            snapshot.put("callable", isCallableTarget(value));
            Map<String, Object> shape = callableShape(value);
            if (shape != null) {
                snapshot.put("callableShape", shape);
            }
        }
        return jsonValue(snapshot);
    }

    public static Object callManifest(String func, Object... args) {
        return bridgeManifestOp("{\"func\":" + jsonScalar(func) + ",\"args\":" + jsonArray(args) + "}");
    }

    private static Object encodeArg(Object value) {
        if (value == null
            || value instanceof Number
            || value instanceof Boolean
            || value instanceof String
            || value instanceof Character) {
            return value;
        }
        if (value instanceof HandleProxy proxy) {
            return proxy.value;
        }
        if (value instanceof StreamProxy proxy) {
            return proxy.value;
        }
        String id = "arg_" + argRefCounter.incrementAndGet();
        argRefs.put(id, value);
        Map<String, Object> descriptor = new LinkedHashMap<>();
        descriptor.put("__omnivm_runtime_ref__", true);
        descriptor.put("runtime", "java");
        descriptor.put("var", "__omnivm_arg_refs[\"" + id + "\"]");
        descriptor.put("callable", isCallableTarget(value));
        return descriptor;
    }

    public static Object proxyGet(Object target, Object keyValue) {
        String key = proxyKey(keyValue);
        if (target == null || key == null) {
            return null;
        }
        if (target instanceof HandleProxy proxy) {
            return proxy.get(key);
        }
        if (target instanceof Map<?, ?> map) {
            return map.get(key);
        }
        if (target instanceof List<?> list && isIntegerKey(key)) {
            int idx = Integer.parseInt(key);
            return idx >= 0 && idx < list.size() ? list.get(idx) : null;
        }
        try {
            java.lang.reflect.Field field = proxyField(target.getClass(), key);
            if (field == null) {
                throw new NoSuchFieldException(key);
            }
            return field.get(target);
        } catch (ReflectiveOperationException ignored) {
        }
        if (key.isEmpty()) {
            return null;
        }
        String getter = "get" + Character.toUpperCase(key.charAt(0)) + key.substring(1);
        for (java.lang.reflect.Method method : proxyMethods(target.getClass())) {
            if (!method.getName().equals(getter)) {
                continue;
            }
            if (method.getParameterCount() == 0) {
                try {
                    return invokeProxyMethod(method, target);
                } catch (ReflectiveOperationException ignored) {
                }
            }
        }
        return null;
    }

    public static Object proxyIndex(Object target, Object key) {
        if (target == null) {
            return null;
        }
        if (target instanceof Map<?, ?> map) {
            return map.get(key);
        }
        Integer idx = numericIndex(key);
        if (idx != null) {
            if (target instanceof List<?> list) {
                return idx >= 0 && idx < list.size() ? list.get(idx) : null;
            }
            if (target.getClass().isArray()) {
                int n = java.lang.reflect.Array.getLength(target);
                return idx >= 0 && idx < n ? java.lang.reflect.Array.get(target, idx) : null;
            }
            if (target instanceof CharSequence chars) {
                return idx >= 0 && idx < chars.length() ? String.valueOf(chars.charAt(idx)) : null;
            }
        }
        return proxyGet(target, String.valueOf(key));
    }

    @SuppressWarnings({"unchecked", "rawtypes"})
    public static boolean proxySet(Object target, Object keyValue, Object value) {
        String key = proxyKey(keyValue);
        if (target == null || key == null) {
            return false;
        }
        if (target instanceof HandleProxy proxy) {
            return proxy.set(key, value);
        }
        if (target instanceof Map map) {
            map.put(key, value);
            return true;
        }
        if (target instanceof List list && "length".equals(key)) {
            Integer nextSize = numericIndex(value);
            if (nextSize == null || nextSize < 0) {
                return false;
            }
            while (list.size() > nextSize) {
                list.remove(list.size() - 1);
            }
            while (list.size() < nextSize) {
                list.add(null);
            }
            return true;
        }
        if (target instanceof List list && isIntegerKey(key)) {
            int idx = Integer.parseInt(key);
            if (idx >= 0 && idx < list.size()) {
                list.set(idx, value);
                return true;
            }
        }
        try {
            java.lang.reflect.Field field = proxyField(target.getClass(), key);
            if (field == null) {
                throw new NoSuchFieldException(key);
            }
            field.set(target, coerceArg(value, field.getType()));
            return true;
        } catch (ReflectiveOperationException ignored) {
        }
        if (key.isEmpty()) {
            return false;
        }
        String setter = "set" + Character.toUpperCase(key.charAt(0)) + key.substring(1);
        for (java.lang.reflect.Method method : proxyMethods(target.getClass())) {
            if (method.getName().equals(setter) && method.getParameterCount() == 1) {
                try {
                    invokeProxyMethod(method, target, coerceArg(value, method.getParameterTypes()[0]));
                    return true;
                } catch (ReflectiveOperationException ignored) {
                }
            }
        }
        return false;
    }

    public static int proxyLen(Object target) {
        if (target == null) {
            return 0;
        }
        if (target instanceof HandleProxy proxy) {
            return proxy.size();
        }
        if (target instanceof Map<?, ?> map) {
            return map.size();
        }
        if (target instanceof Collection<?> collection) {
            return collection.size();
        }
        if (target.getClass().isArray()) {
            return java.lang.reflect.Array.getLength(target);
        }
        if (target instanceof CharSequence chars) {
            return chars.length();
        }
        return 0;
    }

    public static List<Object> proxyIter(Object target, String mode) {
        List<Object> out = new ArrayList<>();
        if (target == null) {
            return out;
        }
        if (target instanceof Map<?, ?> map) {
            if ("items".equals(mode)) {
                for (Map.Entry<?, ?> entry : map.entrySet()) {
                    out.add(List.of(entry.getKey(), entry.getValue()));
                }
            } else if ("keys".equals(mode)) {
                out.addAll(map.keySet());
            } else {
                out.addAll(map.values());
            }
            return out;
        }
        if (target instanceof Iterable<?> iterable) {
            int i = 0;
            for (Object item : iterable) {
                if ("items".equals(mode)) {
                    out.add(List.of(i, item));
                } else if ("keys".equals(mode)) {
                    out.add(i);
                } else {
                    out.add(item);
                }
                i++;
            }
            return out;
        }
        if (target.getClass().isArray()) {
            int n = java.lang.reflect.Array.getLength(target);
            for (int i = 0; i < n; i++) {
                Object item = java.lang.reflect.Array.get(target, i);
                if ("items".equals(mode)) {
                    out.add(List.of(i, item));
                } else if ("keys".equals(mode)) {
                    out.add(i);
                } else {
                    out.add(item);
                }
            }
        }
        return out;
    }

    public static List<Object> proxyKeys(Object target) {
        return proxyIter(target, "keys");
    }

    public static List<Object> proxyValues(Object target) {
        return proxyIter(target, "values");
    }

    public static List<Object> proxyItems(Object target) {
        return proxyIter(target, "items");
    }

    public static boolean proxyContains(Object target, Object key) {
        if (target == null) {
            return false;
        }
        if (target instanceof Map<?, ?> map) {
            return map.containsKey(key);
        }
        Integer idx = numericIndex(key);
        if (idx != null) {
            if (target instanceof List<?> list) {
                return idx >= 0 && idx < list.size();
            }
            if (target.getClass().isArray()) {
                int n = java.lang.reflect.Array.getLength(target);
                return idx >= 0 && idx < n;
            }
        }
        if (target instanceof Collection<?> collection) {
            return collection.contains(key);
        }
        if (target instanceof CharSequence chars && key != null) {
            return chars.toString().contains(String.valueOf(key));
        }
        return proxyGet(target, String.valueOf(key)) != null;
    }

    public static boolean proxyClose(Object target) {
        if (target == null) {
            return false;
        }
        if (target instanceof HandleProxy proxy) {
            return proxy.releaseExplicit();
        }
        if (target instanceof StreamProxy proxy) {
            return proxy.cancel();
        }
        if (target instanceof BufferOwner owner) {
            return owner.release();
        }
        if (target instanceof AutoCloseable closeable) {
            try {
                closeable.close();
                return true;
            } catch (Exception err) {
                throw new RuntimeException("proxy close failed", err);
            }
        }
        Object closeResult = invokePublicProxyLifecycleMethod(target, "close");
        if (closeResult != PROXY_LIFECYCLE_METHOD_MISSING) {
            return !(closeResult instanceof Boolean) || Boolean.TRUE.equals(closeResult);
        }
        Object disposeResult = invokePublicProxyLifecycleMethod(target, "dispose");
        if (disposeResult != PROXY_LIFECYCLE_METHOD_MISSING) {
            return !(disposeResult instanceof Boolean) || Boolean.TRUE.equals(disposeResult);
        }
        return false;
    }

    public static boolean omnivmClose(Object target) {
        return proxyClose(target);
    }

    private static Object invokePublicProxyLifecycleMethod(Object target, String name) {
        for (java.lang.reflect.Method method : proxyMethods(target.getClass())) {
            if (method.getName().equals(name) && method.getParameterCount() == 0
                && java.lang.reflect.Modifier.isPublic(method.getModifiers())) {
                try {
                    Object result = invokeProxyMethod(method, target);
                    return result;
                } catch (ReflectiveOperationException err) {
                    throw new RuntimeException("proxy close failed", err);
                }
            }
        }
        return PROXY_LIFECYCLE_METHOD_MISSING;
    }

    public static boolean proxyCallable(Object target, Object keyValue) {
        String key = proxyKey(keyValue);
        if (target == null) {
            return false;
        }
        if (key == null || key.isEmpty()) {
            return isCallableTarget(target);
        }
        for (java.lang.reflect.Method method : proxyMethods(target.getClass())) {
            if (method.getName().equals(key)) {
                return true;
            }
        }
        Object value = proxyGet(target, key);
        return isCallableTarget(value);
    }

    public static boolean proxyZeroArgCallable(Object target, Object keyValue) {
        String key = proxyKey(keyValue);
        if (target == null || key == null || key.isEmpty()) {
            return false;
        }
        for (java.lang.reflect.Method method : proxyMethods(target.getClass())) {
            if (method.getName().equals(key) && method.getParameterCount() == 0) {
                return true;
            }
        }
        Object value = proxyGet(target, key);
        java.lang.reflect.Method method = functionalMethod(value);
        return method != null && method.getParameterCount() == 0;
    }

    public static Object proxyCall(Object target, Object keyValue, Object argsValue) {
        String key = proxyKey(keyValue);
        List<?> args = argsValue instanceof List<?> list ? list : Collections.emptyList();
        if (target == null) {
            return null;
        }
        if (target instanceof HandleProxy proxy) {
            if (key == null || key.isEmpty()) {
                return proxy.apply(args.toArray());
            }
            return proxy.call(key, args.toArray());
        }
        if (key == null || key.isEmpty()) {
            return invokeCallableTarget(target, args);
        }
        List<InvocationCandidate> candidates = new ArrayList<>();
        for (java.lang.reflect.Method method : proxyMethods(target.getClass())) {
            if (!method.getName().equals(key) || method.getParameterCount() != args.size()) {
                continue;
            }
            InvocationCandidate candidate = invocationCandidate(method, args);
            if (candidate != null) {
                candidates.add(candidate);
            }
        }
        candidates.sort((left, right) -> Integer.compare(left.score, right.score));
        for (InvocationCandidate candidate : candidates) {
            try {
                return invokeProxyMethod(candidate.method, target, candidate.args);
            } catch (ReflectiveOperationException | IllegalArgumentException ignored) {
            }
        }
        Object value = proxyGet(target, key);
        return invokeCallableTarget(value, args);
    }

    private static boolean isCallableTarget(Object target) {
        return target != null && functionalMethod(target) != null;
    }

    private static String proxyKey(Object key) {
        return key == null ? null : String.valueOf(key);
    }

    private static Object invokeCallableTarget(Object target, List<?> args) {
        java.lang.reflect.Method method = functionalMethod(target);
        if (method == null || method.getParameterCount() != args.size()) {
            return null;
        }
        InvocationCandidate candidate = invocationCandidate(method, args);
        if (candidate == null) {
            return null;
        }
        try {
            try {
                method.setAccessible(true);
            } catch (Throwable ignored) {
            }
            return method.invoke(target, candidate.args);
        } catch (ReflectiveOperationException | IllegalArgumentException ignored) {
            return null;
        }
    }

    private static Map<String, Object> callableShape(Object value) {
        Map<String, Object> shape = new LinkedHashMap<>();
        if (value instanceof Class<?> type) {
            Map<String, Object> adapter = javaAdapterForType(type);
            if (adapter != null) {
                shape.put("javaAdapter", adapter);
                return shape;
            }
            return null;
        }
        java.lang.reflect.Method method = functionalMethod(value);
        if (method == null) {
            if (value != null) {
                for (java.lang.reflect.Method candidate : proxyMethods(value.getClass())) {
                    if (candidate.getParameterCount() != 1) {
                        continue;
                    }
                    Map<String, Object> adapter = javaAdapterForType(candidate.getParameterTypes()[0]);
                    if (adapter == null) {
                        continue;
                    }
                    adapter.put("method", candidate.getName());
                    shape.put("javaAdapter", adapter);
                    return shape;
                }
            }
            return null;
        }
        shape.put("arity", method.getParameterCount());
        List<String> parameterNames = new ArrayList<>();
        for (java.lang.reflect.Parameter parameter : method.getParameters()) {
            parameterNames.add(parameter.getName());
        }
        if (!parameterNames.isEmpty()) {
            shape.put("parameterNames", parameterNames);
        }
        if (method.getParameterCount() == 1) {
            Map<String, Object> adapter = javaAdapterForType(method.getParameterTypes()[0]);
            if (adapter != null) {
                shape.put("javaAdapter", adapter);
            }
        }
        return shape;
    }

    private static Map<String, Object> javaAdapterForType(Class<?> type) {
        if (type == null) {
            return null;
        }
        Map<String, Object> adapter = new LinkedHashMap<>();
        adapter.put("targetType", type.getName());
        if (Map.class.isAssignableFrom(type)) {
            adapter.put("kind", "map");
            return adapter;
        }
        if (type.isRecord()) {
            adapter.put("kind", "record");
            List<String> keys = new ArrayList<>();
            for (java.lang.reflect.RecordComponent component : type.getRecordComponents()) {
                keys.add(component.getName());
            }
            adapter.put("keys", keys);
            return adapter;
        }
        List<String> builderKeys = builderKeys(type);
        if (!builderKeys.isEmpty()) {
            adapter.put("kind", "builder");
            adapter.put("keys", builderKeys);
            return adapter;
        }
        return null;
    }

    private static List<String> builderKeys(Class<?> type) {
        List<String> keys = new ArrayList<>();
        for (java.lang.reflect.Method method : proxyMethods(type)) {
            if (method.getParameterCount() != 1) {
                continue;
            }
            String key = builderSetterKey(type, method);
            if (key != null && !keys.contains(key)) {
                keys.add(key);
            }
        }
        return keys;
    }

    private static java.lang.reflect.Method builderSetter(Class<?> type, String key) {
        for (java.lang.reflect.Method method : proxyMethods(type)) {
            if (method.getParameterCount() != 1) {
                continue;
            }
            String methodKey = builderSetterKey(type, method);
            if (key.equals(methodKey)) {
                return method;
            }
        }
        return null;
    }

    private static String builderSetterKey(Class<?> type, java.lang.reflect.Method method) {
        Class<?> returns = method.getReturnType();
        if (!(returns == Void.TYPE || returns == type || type.isAssignableFrom(returns))) {
            return null;
        }
        String name = method.getName();
        if (name.startsWith("set") && name.length() > 3 && Character.isUpperCase(name.charAt(3))) {
            return Character.toLowerCase(name.charAt(3)) + name.substring(4);
        }
        if (!name.equals("build") && !name.equals("getClass")) {
            return name;
        }
        return null;
    }

    private static java.lang.reflect.Method zeroArgMethod(Class<?> type, String name) {
        for (java.lang.reflect.Method method : proxyMethods(type)) {
            if (method.getParameterCount() == 0 && method.getName().equals(name)) {
                return method;
            }
        }
        return null;
    }

    private static Map<String, Object> coerceStringMap(Object value) {
        Map<String, Object> out = new LinkedHashMap<>();
        if (value instanceof Map<?, ?> map) {
            for (Map.Entry<?, ?> entry : map.entrySet()) {
                out.put(String.valueOf(entry.getKey()), entry.getValue());
            }
        }
        return out;
    }

    private static List<String> coerceStringList(Object value) {
        List<String> out = new ArrayList<>();
        if (value instanceof Iterable<?> iterable) {
            for (Object item : iterable) {
                out.add(String.valueOf(item));
            }
        } else if (value != null && value.getClass().isArray()) {
            int n = java.lang.reflect.Array.getLength(value);
            for (int i = 0; i < n; i++) {
                out.add(String.valueOf(java.lang.reflect.Array.get(value, i)));
            }
        }
        return out;
    }

    private static java.lang.reflect.Field proxyField(Class<?> type, String name) {
        try {
            java.lang.reflect.Field field = type.getField(name);
            makeAccessible(field);
            return field;
        } catch (ReflectiveOperationException ignored) {
        }
        for (Class<?> current = type; current != null; current = current.getSuperclass()) {
            try {
                java.lang.reflect.Field field = current.getDeclaredField(name);
                makeAccessible(field);
                return field;
            } catch (ReflectiveOperationException | SecurityException ignored) {
            }
        }
        return null;
    }

    private static List<java.lang.reflect.Method> proxyMethods(Class<?> type) {
        List<java.lang.reflect.Method> methods = new ArrayList<>();
        for (java.lang.reflect.Method method : type.getMethods()) {
            addProxyMethod(methods, method);
        }
        for (Class<?> current = type; current != null; current = current.getSuperclass()) {
            for (java.lang.reflect.Method method : current.getDeclaredMethods()) {
                addProxyMethod(methods, method);
            }
        }
        return methods;
    }

    private static void addProxyMethod(List<java.lang.reflect.Method> methods, java.lang.reflect.Method candidate) {
        if (!candidate.isSynthetic() && !candidate.isBridge() && !containsMethodSignature(methods, candidate)) {
            methods.add(candidate);
        }
    }

    private static Object invokeProxyMethod(java.lang.reflect.Method method, Object target, Object... args)
        throws ReflectiveOperationException {
        makeAccessible(method);
        try {
            return method.invoke(target, args);
        } catch (IllegalAccessException denied) {
            java.lang.reflect.Method fallback = accessibleMethod(target.getClass(), method);
            if (fallback != null && fallback != method) {
                return fallback.invoke(target, args);
            }
            throw denied;
        }
    }

    private static void makeAccessible(java.lang.reflect.AccessibleObject object) {
        try {
            object.setAccessible(true);
        } catch (RuntimeException ignored) {
        }
    }

    private static java.lang.reflect.Method accessibleMethod(Class<?> type, java.lang.reflect.Method method) {
        java.lang.reflect.Method found = accessibleInterfaceMethod(type, method.getName(), method.getParameterTypes());
        if (found != null) {
            return found;
        }
        for (Class<?> current = type.getSuperclass(); current != null; current = current.getSuperclass()) {
            try {
                java.lang.reflect.Method candidate = current.getMethod(method.getName(), method.getParameterTypes());
                if (java.lang.reflect.Modifier.isPublic(candidate.getDeclaringClass().getModifiers())) {
                    return candidate;
                }
            } catch (ReflectiveOperationException ignored) {
            }
            found = accessibleInterfaceMethod(current, method.getName(), method.getParameterTypes());
            if (found != null) {
                return found;
            }
        }
        return null;
    }

    private static java.lang.reflect.Method accessibleInterfaceMethod(Class<?> type, String name, Class<?>[] parameterTypes) {
        if (type == null) {
            return null;
        }
        for (Class<?> iface : type.getInterfaces()) {
            try {
                return iface.getMethod(name, parameterTypes);
            } catch (ReflectiveOperationException ignored) {
            }
            java.lang.reflect.Method nested = accessibleInterfaceMethod(iface, name, parameterTypes);
            if (nested != null) {
                return nested;
            }
        }
        return null;
    }

    private static java.lang.reflect.Method functionalMethod(Object target) {
        if (target == null) {
            return null;
        }
        List<java.lang.reflect.Method> methods = new ArrayList<>();
        collectFunctionalMethods(target.getClass(), methods);
        if (methods.size() != 1) {
            return null;
        }
        java.lang.reflect.Method method = methods.get(0);
        try {
            return target.getClass().getMethod(method.getName(), method.getParameterTypes());
        } catch (NoSuchMethodException ignored) {
            return method;
        }
    }

    private static void collectFunctionalMethods(Class<?> type, List<java.lang.reflect.Method> out) {
        if (type == null) {
            return;
        }
        for (Class<?> iface : type.getInterfaces()) {
            collectFunctionalInterfaceMethods(iface, out);
            collectFunctionalMethods(iface, out);
        }
        collectFunctionalMethods(type.getSuperclass(), out);
    }

    private static void collectFunctionalInterfaceMethods(Class<?> iface, List<java.lang.reflect.Method> out) {
        for (java.lang.reflect.Method method : iface.getMethods()) {
            int modifiers = method.getModifiers();
            if (!java.lang.reflect.Modifier.isAbstract(modifiers)
                || method.isSynthetic()
                || method.isBridge()
                || isObjectMethod(method)) {
                continue;
            }
            if (!containsMethodSignature(out, method)) {
                out.add(method);
            }
        }
    }

    private static boolean containsMethodSignature(List<java.lang.reflect.Method> methods, java.lang.reflect.Method candidate) {
        for (java.lang.reflect.Method method : methods) {
            if (method.getName().equals(candidate.getName())
                && java.util.Arrays.equals(method.getParameterTypes(), candidate.getParameterTypes())) {
                return true;
            }
        }
        return false;
    }

    private static boolean isObjectMethod(java.lang.reflect.Method method) {
        try {
            Object.class.getMethod(method.getName(), method.getParameterTypes());
            return true;
        } catch (NoSuchMethodException ignored) {
            return false;
        }
    }

    /**
     * Generic proxy for runtime-owned handle descriptors.
     */
    public static final class HandleProxy extends AbstractMap<String, Object> implements AutoCloseable {
        private static final Set<String> chattyProxyWarned = Collections.newSetFromMap(new java.util.concurrent.ConcurrentHashMap<String, Boolean>());
        private static final int chattyProxyWarnedLimit = 4096;
        private final Map<String, Object> value;
        private final AtomicBoolean released = new AtomicBoolean(false);
        private final Cleaner.Cleanable cleanable;

        private HandleProxy(Map<String, Object> value) {
            this.value = value;
            if (Boolean.TRUE.equals(value.get("transfer"))) {
                adopt(value.get("id"));
            } else {
                retain(value.get("id"));
            }
            this.cleanable = captureCleaner.register(this, new FinalizerState(value.get("id"), released));
        }

        private static boolean retain(Object id) {
            try {
                Object env = parseJson(OmniVM.call("__manifest", "{\"op\":\"handle_retain\",\"id\":" + jsonScalar(id) + "}"));
                if (env instanceof Map<?, ?>) {
                    Map<?, ?> mapped = (Map<?, ?>) env;
                    return Boolean.TRUE.equals(mapped.get("__omnivm_result__")) && Boolean.TRUE.equals(mapped.get("value"));
                }
            } catch (Throwable ignored) {
            }
            return false;
        }

        private static boolean adopt(Object id) {
            try {
                Object env = parseJson(OmniVM.call("__manifest", "{\"op\":\"handle_adopt\",\"id\":" + jsonScalar(id) + "}"));
                if (env instanceof Map<?, ?>) {
                    Map<?, ?> mapped = (Map<?, ?>) env;
                    return Boolean.TRUE.equals(mapped.get("__omnivm_result__")) && Boolean.TRUE.equals(mapped.get("value"));
                }
            } catch (Throwable ignored) {
            }
            return false;
        }

        public Object id() {
            ensureOpen("get");
            record("property");
            return value.get("id");
        }

        public String runtime() {
            ensureOpen("get");
            record("property");
            Object runtime = value.get("runtime");
            return runtime == null ? null : runtime.toString();
        }

        public String kind() {
            ensureOpen("get");
            record("property");
            Object kind = value.get("kind");
            return kind == null ? null : kind.toString();
        }

        public Map<String, Object> asMap() {
            ensureOpen("iterate");
            record("iterate");
            return Collections.unmodifiableMap(value);
        }

        public void releaseFromFinalizer() {
            cleanable.clean();
        }

        public boolean releaseExplicit() {
            Object id = value.get("id");
            if (id == null || !released.compareAndSet(false, true)) {
                return false;
            }
            Object result;
            try {
                result = bridgeManifestOp("{\"op\":\"handle_release_explicit\",\"id\":" + jsonScalar(id) + "}");
            } catch (RuntimeException | Error err) {
                released.set(false);
                throw err;
            }
            if (!Boolean.TRUE.equals(result)) {
                released.set(false);
                return false;
            }
            if (cleanable != null) {
                cleanable.clean();
            }
            return true;
        }

        private boolean isReleased() {
            return released.get();
        }

        @Override
        public void close() {
            releaseExplicit();
        }

        @Override
        public Object get(Object key) {
            ensureOpen("get");
            if (isIndexedDescriptor() && numericIndex(key) != null) {
                try {
                    return index(key);
                } catch (RuntimeException err) {
                    if (!isMissingBridgeError(err)) {
                        throw err;
                    }
                    return null;
                }
            }
            if (hasLocalValue(key)) {
                return localValue(key);
            }
            Map<?, ?> report = record("property");
            if (isChatty(report)) {
                materializeChatty();
                if (hasLocalValue(key)) {
                    return localValue(key);
                }
            }
            String textKey = String.valueOf(key);
            Object value;
            try {
                value = bridgeGet(textKey);
            } catch (RuntimeException err) {
                if (!isMissingBridgeError(err)) {
                    throw err;
                }
                return null;
            }
            if (isZeroArgCallableDescriptor(value)) {
                return call(textKey);
            }
            return value;
        }

        private boolean isIndexedDescriptor() {
            return Boolean.TRUE.equals(value.get("__omnivm_table__")) || "sequence".equals(String.valueOf(value.get("kind")));
        }

        public Object index(Object key) {
            ensureOpen("index");
            if (hasLocalValue(key)) {
                return localValue(key);
            }
            Map<?, ?> report = record("index");
            if (isChatty(report)) {
                materializeChatty();
                if (hasLocalValue(key)) {
                    return localValue(key);
                }
            }
            return bridgeOp("{\"op\":\"handle_index\",\"id\":" + jsonScalar(value.get("id")) + ",\"value\":" + jsonValue(key) + "}");
        }

        public boolean set(String key, Object next) {
            ensureOpen("set");
            record("mutation");
            Object result = bridgeOp("{\"op\":\"handle_set\",\"id\":" + jsonScalar(value.get("id")) + ",\"key\":\"" + jsonEscape(key) + "\",\"value\":" + jsonValue(encodeArg(next)) + "}");
            return Boolean.TRUE.equals(result);
        }

        public Object call(String key, Object... args) {
            ensureOpen("call");
            record("call");
            return bridgeOp("{\"op\":\"handle_call\",\"id\":" + jsonScalar(value.get("id")) + ",\"key\":\"" + jsonEscape(key) + "\",\"args\":" + jsonArray(args) + "}");
        }

        public Object apply(Object... args) {
            ensureOpen("call");
            record("call");
            return bridgeOp("{\"op\":\"handle_call\",\"id\":" + jsonScalar(value.get("id")) + ",\"key\":\"\",\"args\":" + jsonArray(args) + "}");
        }

        @Override
        public int size() {
            ensureOpen("len");
            try {
                Object length = bridgeOp("{\"op\":\"handle_len\",\"id\":" + jsonScalar(value.get("id")) + "}");
                if (length instanceof Number) {
                    return ((Number) length).intValue();
                }
            } catch (RuntimeException err) {
                if (!isMissingBridgeError(err)) {
                    throw err;
                }
            }
            record("property");
            return value.size();
        }

        @Override
        @SuppressWarnings("unchecked")
        public Collection<Object> values() {
            ensureOpen("iterate");
            try {
                Object values = bridgeOp("{\"op\":\"handle_iter\",\"id\":" + jsonScalar(value.get("id")) + ",\"mode\":\"values\"}");
                if (values instanceof List<?>) {
                    return Collections.unmodifiableList((List<Object>) values);
                }
            } catch (RuntimeException err) {
                if (!isMissingBridgeError(err)) {
                    throw err;
                }
            }
            return super.values();
        }

        @Override
        public Set<Entry<String, Object>> entrySet() {
            ensureOpen("iterate");
            try {
                Object items = bridgeOp("{\"op\":\"handle_iter\",\"id\":" + jsonScalar(value.get("id")) + ",\"mode\":\"items\"}");
                if (items instanceof List<?>) {
                    LinkedHashSet<Entry<String, Object>> entries = new LinkedHashSet<>();
                    for (Object item : (List<?>) items) {
                        if (item instanceof List<?> pair && pair.size() == 2) {
                            entries.add(new SimpleImmutableEntry<>(String.valueOf(pair.get(0)), pair.get(1)));
                        }
                    }
                    return Collections.unmodifiableSet(entries);
                }
            } catch (RuntimeException err) {
                if (!isMissingBridgeError(err)) {
                    throw err;
                }
            }
            record("iterate");
            return Collections.unmodifiableMap(value).entrySet();
        }

        @Override
        public boolean containsKey(Object key) {
            ensureOpen("contains");
            try {
                Object contains = bridgeOp("{\"op\":\"handle_contains\",\"id\":" + jsonScalar(value.get("id")) + ",\"value\":" + jsonValue(key) + "}");
                if (contains instanceof Boolean) {
                    return Boolean.TRUE.equals(contains);
                }
            } catch (RuntimeException err) {
                if (!isMissingBridgeError(err)) {
                    throw err;
                }
            }
            record("property");
            return hasLocalValue(key);
        }

        @Override
        public String toString() {
            if (released.get()) {
                return value.toString();
            }
            if (hasLocalValue("toString")) {
                return String.valueOf(localValue("toString"));
            }
            try {
                return String.valueOf(bridgeGet("toString"));
            } catch (RuntimeException err) {
                if (!isMissingBridgeError(err)) {
                    throw err;
                }
            }
            return value.toString();
        }

        private RuntimeError closedOperationError(String op) {
            Object rawID = value.get("id");
            Object rawKind = value.get("kind");
            String kind = rawKind == null ? "object" : String.valueOf(rawKind);
            Object rawRuntime = value.get("runtime");
            String runtime = rawRuntime == null ? "unknown" : String.valueOf(rawRuntime);
            String suffix = rawID == null ? "" : " #" + rawID;
            Map<String, Object> proxy = ownerDispatchMap(
                "id", rawID,
                "runtime", runtime,
                "kind", kind,
                "closed", true);
            return runtimeError(
                "OmniVM Java handle proxy " + op + " on closed " + kind + " handle" + suffix,
                "proxy_lifecycle",
                ownerDispatchMap("proxy", proxy));
        }

        private void ensureOpen(String op) {
            if (released.get()) {
                throw closedOperationError(op);
            }
        }

        private Map<?, ?> record(String kind) {
            if (released.get()) {
                return null;
            }
            Object id = value.get("id");
            if (id == null) {
                return null;
            }
            try {
                Object env = parseJson(OmniVM.call("__manifest", "{\"op\":\"handle_access\",\"id\":" + jsonScalar(id) + ",\"kind\":\"" + jsonEscape(kind) + "\"}"));
                if (!(env instanceof Map)) {
                    return null;
                }
                Object reportValue = ((Map<?, ?>) env).get("value");
                if (!(reportValue instanceof Map)) {
                    return null;
                }
                Map<?, ?> report = (Map<?, ?>) reportValue;
                if (isChatty(report)) {
                    warnChatty(report);
                }
                return report;
            } catch (Throwable ignored) {
            }
            return null;
        }

        private boolean isChatty(Map<?, ?> report) {
            return report != null && Boolean.TRUE.equals(report.get("chatty"));
        }

        private boolean isInternalDescriptorKey(Object key) {
            if (!isDescriptorValue() || key == null) {
                return false;
            }
            String text = String.valueOf(key);
            return "__omnivm_resource__".equals(text)
                || "__omnivm_table__".equals(text)
                || "__omnivm_job__".equals(text)
                || "id".equals(text)
                || "runtime".equals(text)
                || "kind".equals(text)
                || "closed".equals(text)
                || "transfer".equals(text)
                || "disposer".equals(text)
                || "format".equals(text)
                || "ownership".equals(text)
                || "metadata".equals(text)
                || "buffer".equals(text)
                || "released".equals(text)
                || "done".equals(text)
                || "cancelled".equals(text)
                || "cancelReason".equals(text)
                || "payload".equals(text)
                || "result".equals(text)
                || "__omnivm_materialized__".equals(text);
        }

        private boolean isDescriptorValue() {
            return Boolean.TRUE.equals(value.get("__omnivm_resource__"))
                || Boolean.TRUE.equals(value.get("__omnivm_table__"))
                || Boolean.TRUE.equals(value.get("__omnivm_job__"));
        }

        private boolean hasLocalValue(Object key) {
            if (key == null) {
                return false;
            }
            if (value.containsKey(key) && !isInternalDescriptorKey(key)) {
                return true;
            }
            String textKey = String.valueOf(key);
            return value.containsKey(textKey) && !isInternalDescriptorKey(textKey);
        }

        private Object localValue(Object key) {
            if (value.containsKey(key) && !isInternalDescriptorKey(key)) {
                return value.get(key);
            }
            String textKey = String.valueOf(key);
            if (value.containsKey(textKey) && !isInternalDescriptorKey(textKey)) {
                return value.get(textKey);
            }
            return null;
        }

        private void materializeChatty() {
            ensureOpen("iterate");
            if (Boolean.TRUE.equals(value.get("__omnivm_materialized__"))) {
                return;
            }
            Object items;
            try {
                items = bridgeOp("{\"op\":\"handle_iter\",\"id\":" + jsonScalar(value.get("id")) + ",\"mode\":\"items\",\"materialize\":true}");
            } catch (RuntimeException err) {
                if (!isMissingBridgeError(err)) {
                    throw err;
                }
                return;
            }
            if (!(items instanceof List<?>)) {
                return;
            }
            for (Object item : (List<?>) items) {
                if (item instanceof List<?> pair && pair.size() >= 2) {
                    String key = String.valueOf(pair.get(0));
                    if (!value.containsKey(key)) {
                        value.put(key, pair.get(1));
                    }
                }
            }
            value.put("__omnivm_materialized__", true);
        }

        private void warnChatty(Map<?, ?> report) {
            Object rawID = report.get("id");
            if (rawID == null) {
                return;
            }
            String id = String.valueOf(rawID);
            if (chattyProxyWarned.add(id)) {
                if (chattyProxyWarned.size() > chattyProxyWarnedLimit) {
                    chattyProxyWarned.clear();
                    chattyProxyWarned.add(id);
                }
                Object rawKind = report.get("chattiest_access_kind");
                if (rawKind == null) {
                    rawKind = report.get("access_kind");
                }
                String accessKind = rawKind == null ? "access" : String.valueOf(rawKind);
                System.err.println("omnivm: chatty cross-runtime proxy access detected for handle " + id + " (" + accessKind + "); consider runtime-local iteration or bulk materialization");
            }
        }

        private boolean isMissingBridgeError(RuntimeException err) {
            String text = String.valueOf(err.getMessage());
            return text.contains(" has no property ")
                || text.contains(" has no index ")
                || text.contains(" has no length")
                || text.contains(" is not iterable")
                || text.contains(" does not support contains")
                || text.contains(" has no writable property ");
        }

        @SuppressWarnings("unchecked")
        private Object bridgeGet(String key) {
            Object id = value.get("id");
            if (id == null || key == null) {
                return null;
            }
            return bridgeOp("{\"op\":\"handle_get\",\"id\":" + jsonScalar(id) + ",\"key\":\"" + jsonEscape(key) + "\"}");
        }

        private boolean isZeroArgCallableDescriptor(Object value) {
            if (!(value instanceof Map<?, ?> map)) {
                return false;
            }
            return Boolean.TRUE.equals(map.get("__omnivm_callable__")) && Boolean.TRUE.equals(map.get("zeroArg"));
        }

        @SuppressWarnings("unchecked")
        private Object bridgeOp(String payload) {
            try {
                String raw = OmniVM.call("__manifest", payload);
                if (raw != null && raw.startsWith("ERR:")) {
                    throw new RuntimeException(raw.substring(4));
                }
                Object parsed = parseJson(raw);
                if (parsed instanceof Map<?, ?> env && Boolean.TRUE.equals(env.get("__omnivm_result__"))) {
                    return materializeCapture(((Map<String, Object>) env).get("value"));
                }
                throw new RuntimeException("manifest bridge returned non-result: " + raw);
            } catch (Throwable ignored) {
                if (ignored instanceof RuntimeException runtimeError) {
                    throw runtimeError;
                }
                throw new RuntimeException("manifest bridge failed", ignored);
            }
        }
    }

    private static final class FinalizerState implements Runnable {
        private final Object id;
        private final AtomicBoolean released;

        private FinalizerState(Object id, AtomicBoolean released) {
            this.id = id;
            this.released = released;
        }

        @Override
        public void run() {
            if (id == null || !released.compareAndSet(false, true)) {
                return;
            }
            try {
                OmniVM.call("__manifest", "{\"op\":\"handle_release_finalizer\",\"id\":" + jsonScalar(id) + "}");
            } catch (Throwable ignored) {
            }
        }
    }

    public static final class FlowPublisherIterator implements Iterator<Object>, AutoCloseable, Flow.Subscriber<Object> {
        private final Object doneSignal = new Object();
        private final Object nullSignal = new Object();
        private final BlockingQueue<Object> queue = new ArrayBlockingQueue<>(2);
        private final CountDownLatch subscribed = new CountDownLatch(1);
        private final AtomicReference<Throwable> error = new AtomicReference<>();
        private final AtomicLong requested = new AtomicLong(0);
        private final AtomicBoolean terminalSignalled = new AtomicBoolean(false);
        private final AtomicBoolean closeSignalled = new AtomicBoolean(false);
        private volatile Flow.Subscription subscription;
        private volatile boolean closed;
        private boolean loaded;
        private boolean finished;
        private Object item;

        @SuppressWarnings({"rawtypes", "unchecked"})
        public FlowPublisherIterator(Flow.Publisher<?> publisher) {
            ((Flow.Publisher) publisher).subscribe(this);
        }

        @Override
        public void onSubscribe(Flow.Subscription subscription) {
            if (this.subscription != null) {
                subscription.cancel();
                return;
            }
            if (closed) {
                subscription.cancel();
                subscribed.countDown();
                return;
            }
            this.subscription = subscription;
            subscribed.countDown();
        }

        @Override
        public void onNext(Object item) {
            if (closed || terminalSignalled.get()) {
                return;
            }
            if (!claimRequested()) {
                failProtocol(new IllegalStateException("Flow.Publisher emitted without demand"));
                return;
            }
            if (!queue.offer(item == null ? nullSignal : item)) {
                failProtocol(new IllegalStateException("Flow.Publisher exceeded OmniVM stream backpressure buffer"));
            }
        }

        @Override
        public void onError(Throwable error) {
            signalDone(error, false);
        }

        @Override
        public void onComplete() {
            signalDone(null, false);
        }

        private boolean claimRequested() {
            while (true) {
                long current = requested.get();
                if (current <= 0) {
                    return false;
                }
                if (requested.compareAndSet(current, current - 1)) {
                    return true;
                }
            }
        }

        private void failProtocol(Throwable failure) {
            Flow.Subscription current = subscription;
            if (current != null) {
                current.cancel();
            }
            signalDone(failure, true);
        }

        private void signalDone(Throwable failure, boolean discardPending) {
            if (failure != null) {
                error.compareAndSet(null, failure);
            }
            if (!terminalSignalled.compareAndSet(false, true)) {
                return;
            }
            if (discardPending) {
                queue.clear();
            }
            if (!queue.offer(doneSignal)) {
                queue.clear();
                queue.offer(doneSignal);
            }
        }

        private void load() {
            if (loaded || finished) {
                return;
            }
            if (closed) {
                finished = true;
                return;
            }
            try {
                subscribed.await();
                Flow.Subscription current = subscription;
                if (current == null) {
                    finished = true;
                    return;
                }
                if (!terminalSignalled.get()) {
                    requested.incrementAndGet();
                    current.request(1);
                }
                Object next = queue.take();
                if (next == doneSignal) {
                    finished = true;
                    Throwable currentError = error.get();
                    if (currentError != null) {
                        throw new RuntimeException(currentError);
                    }
                    return;
                }
                item = next == nullSignal ? null : next;
                loaded = true;
            } catch (InterruptedException interrupted) {
                Thread.currentThread().interrupt();
                throw new RuntimeException(interrupted);
            }
        }

        @Override
        public boolean hasNext() {
            load();
            return !finished;
        }

        @Override
        public Object next() {
            if (!hasNext()) {
                throw new NoSuchElementException();
            }
            Object out = item;
            item = null;
            loaded = false;
            return out;
        }

        @Override
        public void close() {
            if (!closeSignalled.compareAndSet(false, true)) {
                return;
            }
            closed = true;
            item = null;
            loaded = false;
            finished = true;
            Flow.Subscription current = subscription;
            if (current != null) {
                current.cancel();
            }
            subscribed.countDown();
            signalDone(null, true);
        }
    }

    public static final class StreamProxy implements Iterable<Object>, AutoCloseable {
        private final Map<String, Object> value;
        private final List<?> localValues;
        private final AtomicBoolean released = new AtomicBoolean(false);
        private final Cleaner.Cleanable cleanable;

        private StreamProxy(Map<String, Object> value) {
            this.value = value;
            Object values = value.get("values");
            this.localValues = values instanceof List<?> ? (List<?>) values : null;
            Object id = value.get("id");
            if (id != null && Boolean.TRUE.equals(value.get("transfer"))) {
                HandleProxy.adopt(id);
            } else if (id != null) {
                HandleProxy.retain(id);
            }
            if (id != null) {
                this.cleanable = captureCleaner.register(this, new FinalizerState(id, released));
            } else {
                this.cleanable = null;
            }
        }

        public void releaseFromFinalizer() {
            if (cleanable != null) {
                cleanable.clean();
            } else {
                markReleased();
            }
        }

        public boolean releaseExplicit() {
            return cancel();
        }

        private boolean markReleased() {
            if (!released.compareAndSet(false, true)) {
                return false;
            }
            if (cleanable != null) {
                cleanable.clean();
            }
            return true;
        }

        private boolean isReleased() {
            return released.get();
        }

        public boolean cancel() {
            if (localValues != null) {
                return markReleased();
            }
            Object id = value.get("id");
            if (id == null || !released.compareAndSet(false, true)) {
                return false;
            }
            Object result;
            try {
                result = bridgeManifestOp("{\"op\":\"stream_cancel\",\"id\":" + jsonScalar(id) + "}");
            } catch (RuntimeException | Error err) {
                released.set(false);
                throw err;
            }
            if (!Boolean.TRUE.equals(result)) {
                released.set(false);
                return false;
            }
            cleanable.clean();
            return true;
        }

        private void cancelAfterLoadFailure(RuntimeException err) {
            try {
                if (!StreamProxy.this.cancel()) {
                    markReleased();
                }
            } catch (RuntimeException closeErr) {
                markReleased();
                err.addSuppressed(closeErr);
            }
        }

        @Override
        public void close() {
            cancel();
        }

        public List<Object> toList() {
            List<Object> out = new ArrayList<>();
            try (Stream<Object> items = stream()) {
                items.forEach(out::add);
            }
            return out;
        }

        public Stream<Object> stream() {
            return StreamSupport.stream(spliterator(), false).onClose(this::close);
        }

        @Override
        public Iterator<Object> iterator() {
            return new Iterator<Object>() {
                private int localIndex;
                private boolean loaded;
                private boolean done;
                private Object next;

                @Override
                public boolean hasNext() {
                    load();
                    return !done;
                }

                @Override
                public Object next() {
                    load();
                    if (done) {
                        throw new NoSuchElementException();
                    }
                    Object out = next;
                    loaded = false;
                    next = null;
                    return out;
                }

                @SuppressWarnings("unchecked")
                private void load() {
                    if (loaded) {
                        return;
                    }
                    loaded = true;
                    if (localValues != null) {
                        if (released.get() || localIndex >= localValues.size()) {
                            done = true;
                            markReleased();
                            return;
                        }
                        try {
                            next = materializeCapture(localValues.get(localIndex++));
                        } catch (RuntimeException err) {
                            done = true;
                            markReleased();
                            throw err;
                        }
                        return;
                    }
                    if (released.get()) {
                        done = true;
                        return;
                    }
                    Object id = value.get("id");
                    Object result;
                    try {
                        result = bridgeManifestOp("{\"op\":\"stream_next\",\"id\":" + jsonScalar(id) + "}");
                    } catch (RuntimeException err) {
                        done = true;
                        cancelAfterLoadFailure(err);
                        throw err;
                    }
                    if (!(result instanceof Map<?, ?> item) || !item.containsKey("done")) {
                        done = true;
                        RuntimeError err = runtimeError(
                            "OmniVM stream_next returned malformed chunk for handle " + String.valueOf(id) + ": expected an object with a done flag",
                            "stream_next",
                            ownerDispatchMap("stream", ownerDispatchMap("id", id, "chunk", result)));
                        cancelAfterLoadFailure(err);
                        throw err;
                    }
                    if (Boolean.TRUE.equals(item.get("done"))) {
                        done = true;
                        markReleased();
                        return;
                    }
                    try {
                        next = materializeStreamChunk(((Map<String, Object>) item).get("value"));
                    } catch (RuntimeException err) {
                        done = true;
                        cancelAfterLoadFailure(err);
                        throw err;
                    }
                }
            };
        }
    }

    @SuppressWarnings("unchecked")
    private static Object materializeStreamChunk(Object value) {
        if (value instanceof HandleProxy || value instanceof StreamProxy) {
            return value;
        }
        if (value instanceof Map<?, ?> rawMap && Boolean.TRUE.equals(rawMap.get("__omnivm_table__"))) {
            Map<String, Object> table = new LinkedHashMap<>();
            for (Map.Entry<?, ?> entry : rawMap.entrySet()) {
                table.put(String.valueOf(entry.getKey()), entry.getValue());
            }
            Object rawMetadata = table.get("metadata");
            Map<String, Object> metadata = rawMetadata instanceof Map<?, ?>
                ? new LinkedHashMap<>()
                : Collections.emptyMap();
            if (rawMetadata instanceof Map<?, ?> metadataMap) {
                for (Map.Entry<?, ?> entry : metadataMap.entrySet()) {
                    metadata.put(String.valueOf(entry.getKey()), entry.getValue());
                }
            }
            Object dtypeValue = metadata.containsKey("dtype") ? metadata.get("dtype") : table.get("dtype");
            Object bufferValue = table.get("buffer") != null ? table.get("buffer") : metadata.get("buffer");
            if (dtypeValue instanceof Number dtype && bufferValue != null && isByteDtype(dtype.intValue())) {
                byte[] raw = getBuffer(String.valueOf(bufferValue));
                if (raw != null) {
                    int length = raw.length;
                    Object shapeValue = metadata.get("shape");
                    if (shapeValue instanceof List<?> shape && !shape.isEmpty() && shape.get(0) instanceof Number n) {
                        length = Math.max(0, n.intValue());
                    }
                    int offset = metadata.get("offset") instanceof Number n ? Math.max(0, n.intValue()) : 0;
                    int stride = 1;
                    Object stridesValue = metadata.get("strides");
                    if (stridesValue instanceof List<?> strides && !strides.isEmpty() && strides.get(0) instanceof Number n) {
                        stride = n.intValue() == 0 ? 1 : n.intValue();
                    }
                    if (length == 0) {
                        return new byte[0];
                    }
                    if (stride == 1 && offset <= raw.length) {
                        int end = Math.min(raw.length, offset + length);
                        byte[] out = new byte[Math.max(0, end - offset)];
                        System.arraycopy(raw, offset, out, 0, out.length);
                        return out;
                    }
                    byte[] out = new byte[length];
                    int written = 0;
                    for (int i = 0; i < length; i++) {
                        int src = offset + i * stride;
                        if (src >= 0 && src < raw.length) {
                            out[written++] = raw[src];
                        }
                    }
                    if (written == out.length) {
                        return out;
                    }
                    byte[] trimmed = new byte[written];
                    System.arraycopy(out, 0, trimmed, 0, written);
                    return trimmed;
                }
            }
        }
        if (value instanceof Map<?, ?> rawMap) {
            byte[] bytes = contiguousByteMap(rawMap);
            if (bytes != null) {
                return bytes;
            }
        }
        return materializeCapture(value);
    }

    private static boolean isByteDtype(int dtype) {
        return dtype == 0 || dtype == 5 || dtype == 10 || dtype == 11;
    }

    private static byte[] contiguousByteMap(Map<?, ?> rawMap) {
        int size = rawMap.size();
        if (size == 0) {
            return null;
        }
        byte[] out = new byte[size];
        for (int i = 0; i < size; i++) {
            Object rawValue = rawMap.get(i);
            if (rawValue == null) {
                rawValue = rawMap.get(String.valueOf(i));
            }
            if (!(rawValue instanceof Number n)) {
                return null;
            }
            int value = n.intValue();
            if (value < 0 || value > 255) {
                return null;
            }
            out[i] = (byte) value;
        }
        for (Object key : rawMap.keySet()) {
            try {
                int idx = Integer.parseInt(String.valueOf(key));
                if (idx < 0 || idx >= size) {
                    return null;
                }
            } catch (NumberFormatException err) {
                return null;
            }
        }
        return out;
    }

    @SuppressWarnings("unchecked")
    private static Object materializeCapture(Object value) {
        if (value instanceof Map<?, ?> rawMap) {
            Map<String, Object> mapped = new LinkedHashMap<>();
            for (Map.Entry<?, ?> entry : rawMap.entrySet()) {
                mapped.put(String.valueOf(entry.getKey()), materializeCapture(entry.getValue()));
            }
            if (isStreamDescriptor(mapped)) {
                return cachedProxy("stream", mapped, true);
            }
            if (isHandleDescriptor(mapped)) {
                return cachedProxy("handle", mapped, false);
            }
            return mapped;
        }
        if (value instanceof List<?> rawList) {
            List<Object> out = new ArrayList<>(rawList.size());
            for (Object item : rawList) {
                out.add(materializeCapture(item));
            }
            return out;
        }
        return value;
    }

    private static Object cachedProxy(String kind, Map<String, Object> value, boolean stream) {
        Object id = value.get("id");
        if (id == null) {
            return stream ? new StreamProxy(value) : new HandleProxy(value);
        }
        String key = kind + ":" + String.valueOf(id);
        WeakReference<Object> ref = proxyCache.get(key);
        Object cached = ref == null ? null : ref.get();
        if ((cached instanceof HandleProxy handleProxy && handleProxy.isReleased())
            || (cached instanceof StreamProxy streamProxy && streamProxy.isReleased())) {
            proxyCache.remove(key, ref);
            cached = null;
        }
        if (cached != null) {
            return cached;
        }
        if (ref != null) {
            proxyCache.remove(key, ref);
        }
        Object proxy = stream ? new StreamProxy(value) : new HandleProxy(value);
        proxyCache.put(key, new WeakReference<>(proxy));
        pruneProxyCache();
        return proxy;
    }

    private static void pruneProxyCache() {
        if (proxyCache.size() <= proxyCachePruneThreshold) {
            return;
        }
        for (Map.Entry<String, WeakReference<Object>> entry : proxyCache.entrySet()) {
            WeakReference<Object> ref = entry.getValue();
            if (ref == null || ref.get() == null) {
                proxyCache.remove(entry.getKey(), ref);
            }
        }
    }

    private static boolean isHandleDescriptor(Map<String, Object> value) {
        return Boolean.TRUE.equals(value.get("__omnivm_resource__"))
            || Boolean.TRUE.equals(value.get("__omnivm_table__"))
            || Boolean.TRUE.equals(value.get("__omnivm_job__"));
    }

    private static boolean isStreamDescriptor(Map<String, Object> value) {
        return Boolean.TRUE.equals(value.get("__omnivm_stream__"))
            || Boolean.TRUE.equals(value.get("__omnivm_channel__"));
    }

    @SuppressWarnings("unchecked")
    private static Object bridgeManifestOp(String payload) {
        try {
            String raw = OmniVM.call("__manifest", payload);
            if (raw != null && raw.startsWith("ERR:")) {
                throw new RuntimeException(raw.substring(4));
            }
            Object parsed = parseJson(raw);
            if (parsed instanceof Map<?, ?> env && Boolean.TRUE.equals(env.get("__omnivm_result__"))) {
                return materializeCapture(((Map<String, Object>) env).get("value"));
            }
            throw new RuntimeException("manifest bridge returned non-result: " + raw);
        } catch (Throwable ignored) {
            if (ignored instanceof RuntimeException runtimeError) {
                throw runtimeError;
            }
            throw new RuntimeException("manifest bridge failed", ignored);
        }
    }

    private static Object parseJson(String json) {
        return new JsonParser(json).parse();
    }

    private static String jsonScalar(Object value) {
        if (value instanceof Number || value instanceof Boolean) {
            return value.toString();
        }
        return "\"" + jsonEscape(String.valueOf(value)) + "\"";
    }

    private static String jsonArray(Object[] values) {
        StringBuilder out = new StringBuilder("[");
        for (int i = 0; i < values.length; i++) {
            if (i > 0) {
                out.append(',');
            }
            out.append(jsonValue(encodeArg(values[i])));
        }
        out.append(']');
        return out.toString();
    }

    @SuppressWarnings("unchecked")
    private static String jsonValue(Object value) {
        if (value == null) {
            return "null";
        }
        if (value instanceof Number || value instanceof Boolean) {
            return value.toString();
        }
        if (value instanceof String) {
            return "\"" + jsonEscape((String) value) + "\"";
        }
        if (value instanceof Character) {
            return "\"" + jsonEscape(String.valueOf(value)) + "\"";
        }
        if (value instanceof Map<?, ?> map) {
            StringBuilder out = new StringBuilder("{");
            boolean first = true;
            for (Map.Entry<?, ?> entry : map.entrySet()) {
                if (!first) {
                    out.append(',');
                }
                first = false;
                out.append('"').append(jsonEscape(String.valueOf(entry.getKey()))).append("\":");
                out.append(jsonValue(entry.getValue()));
            }
            out.append('}');
            return out.toString();
        }
        if (value instanceof Iterable<?> iterable) {
            StringBuilder out = new StringBuilder("[");
            boolean first = true;
            for (Object item : iterable) {
                if (!first) {
                    out.append(',');
                }
                first = false;
                out.append(jsonValue(item));
            }
            out.append(']');
            return out.toString();
        }
        return "\"" + jsonEscape(String.valueOf(value)) + "\"";
    }

    private static String jsonBridgeValue(Object value) {
        if (value == null
            || value instanceof Number
            || value instanceof Boolean
            || value instanceof String
            || value instanceof Map<?, ?>
            || value instanceof Iterable<?>) {
            return jsonValue(value);
        }
        if (value != null && value.getClass().isArray()) {
            List<Object> out = new ArrayList<>();
            int n = java.lang.reflect.Array.getLength(value);
            for (int i = 0; i < n; i++) {
                out.add(java.lang.reflect.Array.get(value, i));
            }
            return jsonValue(out);
        }
        Map<String, Object> descriptor = new LinkedHashMap<>();
        descriptor.put("__omnivm_java_object__", true);
        descriptor.put("class", value.getClass().getName());
        descriptor.put("value", String.valueOf(value));
        return jsonValue(descriptor);
    }

    private static boolean isIntegerKey(String key) {
        if (key == null || key.isEmpty()) {
            return false;
        }
        for (int i = 0; i < key.length(); i++) {
            char ch = key.charAt(i);
            if (i == 0 && ch == '-') {
                continue;
            }
            if (ch < '0' || ch > '9') {
                return false;
            }
        }
        return true;
    }

    private static Integer numericIndex(Object value) {
        if (value instanceof Number number) {
            return number.intValue();
        }
        if (value instanceof String text && isIntegerKey(text)) {
            return Integer.parseInt(text);
        }
        return null;
    }

    private static Object coerceArg(Object value, Class<?> target) {
        if (value == null) {
            return null;
        }
        if (target.isInstance(value)) {
            return value;
        }
        if ((target == int.class || target == Integer.class) && value instanceof Number number) {
            return number.intValue();
        }
        if ((target == long.class || target == Long.class) && value instanceof Number number) {
            return number.longValue();
        }
        if ((target == double.class || target == Double.class) && value instanceof Number number) {
            return number.doubleValue();
        }
        if ((target == float.class || target == Float.class) && value instanceof Number number) {
            return number.floatValue();
        }
        if ((target == byte.class || target == Byte.class) && value instanceof Number number) {
            return number.byteValue();
        }
        if ((target == short.class || target == Short.class) && value instanceof Number number) {
            return number.shortValue();
        }
        if ((target == char.class || target == Character.class) && value instanceof Character ch) {
            return ch;
        }
        if ((target == char.class || target == Character.class) && value instanceof CharSequence text && text.length() == 1) {
            return text.charAt(0);
        }
        if ((target == boolean.class || target == Boolean.class) && value instanceof Boolean bool) {
            return bool;
        }
        if (target == String.class) {
            return String.valueOf(value);
        }
        return value;
    }

    private static InvocationCandidate invocationCandidate(java.lang.reflect.Method method, List<?> args) {
        Class<?>[] types = method.getParameterTypes();
        if (types.length != args.size()) {
            return null;
        }
        Object[] converted = new Object[args.size()];
        int score = 0;
        for (int i = 0; i < args.size(); i++) {
            int argScore = coercionScore(args.get(i), types[i]);
            if (argScore < 0) {
                return null;
            }
            converted[i] = coerceArg(args.get(i), types[i]);
            score += argScore;
        }
        return new InvocationCandidate(method, converted, score);
    }

    private static int coercionScore(Object value, Class<?> target) {
        if (value == null) {
            return target.isPrimitive() ? -1 : 4;
        }
        Class<?> boxedTarget = boxedType(target);
        if (boxedTarget == value.getClass()) {
            return 0;
        }
        if (target.isInstance(value)) {
            return target == Object.class ? 6 : 2;
        }
        if (Number.class.isAssignableFrom(boxedTarget) && value instanceof Number) {
            return 1;
        }
        if (boxedTarget == Character.class && value instanceof CharSequence text && text.length() == 1) {
            return 1;
        }
        if (boxedTarget == Boolean.class && value instanceof Boolean) {
            return 0;
        }
        if (target == String.class) {
            return value instanceof CharSequence ? 1 : 8;
        }
        return -1;
    }

    private static Class<?> boxedType(Class<?> type) {
        if (!type.isPrimitive()) {
            return type;
        }
        if (type == int.class) {
            return Integer.class;
        }
        if (type == long.class) {
            return Long.class;
        }
        if (type == double.class) {
            return Double.class;
        }
        if (type == float.class) {
            return Float.class;
        }
        if (type == boolean.class) {
            return Boolean.class;
        }
        if (type == byte.class) {
            return Byte.class;
        }
        if (type == short.class) {
            return Short.class;
        }
        if (type == char.class) {
            return Character.class;
        }
        return type;
    }

    private static final class InvocationCandidate {
        private final java.lang.reflect.Method method;
        private final Object[] args;
        private final int score;

        private InvocationCandidate(java.lang.reflect.Method method, Object[] args, int score) {
            this.method = method;
            this.args = args;
            this.score = score;
        }
    }

    private static String jsonEscape(String value) {
        return value
            .replace("\\", "\\\\")
            .replace("\"", "\\\"")
            .replace("\n", "\\n")
            .replace("\r", "\\r")
            .replace("\t", "\\t");
    }

    private static final class JsonParser {
        private final String input;
        private int pos;

        private JsonParser(String input) {
            this.input = input == null ? "null" : input;
        }

        private Object parse() {
            Object value = parseValue();
            skipWhitespace();
            if (pos != input.length()) {
                throw new IllegalArgumentException("trailing JSON input at " + pos);
            }
            return value;
        }

        private Object parseValue() {
            skipWhitespace();
            if (pos >= input.length()) {
                throw new IllegalArgumentException("unexpected end of JSON input");
            }
            char ch = input.charAt(pos);
            if (ch == '{') {
                return parseObject();
            }
            if (ch == '[') {
                return parseArray();
            }
            if (ch == '"') {
                return parseString();
            }
            if (input.startsWith("true", pos)) {
                pos += 4;
                return Boolean.TRUE;
            }
            if (input.startsWith("false", pos)) {
                pos += 5;
                return Boolean.FALSE;
            }
            if (input.startsWith("null", pos)) {
                pos += 4;
                return null;
            }
            return parseNumber();
        }

        private Map<String, Object> parseObject() {
            expect('{');
            Map<String, Object> out = new LinkedHashMap<>();
            skipWhitespace();
            if (peek('}')) {
                pos++;
                return out;
            }
            while (true) {
                skipWhitespace();
                String key = parseString();
                skipWhitespace();
                expect(':');
                out.put(key, parseValue());
                skipWhitespace();
                if (peek('}')) {
                    pos++;
                    return out;
                }
                expect(',');
            }
        }

        private List<Object> parseArray() {
            expect('[');
            List<Object> out = new ArrayList<>();
            skipWhitespace();
            if (peek(']')) {
                pos++;
                return out;
            }
            while (true) {
                out.add(parseValue());
                skipWhitespace();
                if (peek(']')) {
                    pos++;
                    return out;
                }
                expect(',');
            }
        }

        private String parseString() {
            expect('"');
            StringBuilder out = new StringBuilder();
            while (pos < input.length()) {
                char ch = input.charAt(pos++);
                if (ch == '"') {
                    return out.toString();
                }
                if (ch != '\\') {
                    out.append(ch);
                    continue;
                }
                if (pos >= input.length()) {
                    throw new IllegalArgumentException("unterminated JSON escape");
                }
                char esc = input.charAt(pos++);
                switch (esc) {
                    case '"':
                    case '\\':
                    case '/':
                        out.append(esc);
                        break;
                    case 'b':
                        out.append('\b');
                        break;
                    case 'f':
                        out.append('\f');
                        break;
                    case 'n':
                        out.append('\n');
                        break;
                    case 'r':
                        out.append('\r');
                        break;
                    case 't':
                        out.append('\t');
                        break;
                    case 'u':
                        if (pos + 4 > input.length()) {
                            throw new IllegalArgumentException("short JSON unicode escape");
                        }
                        out.append((char) Integer.parseInt(input.substring(pos, pos + 4), 16));
                        pos += 4;
                        break;
                    default:
                        throw new IllegalArgumentException("invalid JSON escape \\" + esc);
                }
            }
            throw new IllegalArgumentException("unterminated JSON string");
        }

        private Number parseNumber() {
            int start = pos;
            if (peek('-')) {
                pos++;
            }
            while (pos < input.length() && Character.isDigit(input.charAt(pos))) {
                pos++;
            }
            boolean floating = false;
            if (peek('.')) {
                floating = true;
                pos++;
                while (pos < input.length() && Character.isDigit(input.charAt(pos))) {
                    pos++;
                }
            }
            if (peek('e') || peek('E')) {
                floating = true;
                pos++;
                if (peek('+') || peek('-')) {
                    pos++;
                }
                while (pos < input.length() && Character.isDigit(input.charAt(pos))) {
                    pos++;
                }
            }
            String raw = input.substring(start, pos);
            if (raw.isEmpty() || raw.equals("-")) {
                throw new IllegalArgumentException("invalid JSON number at " + start);
            }
            if (floating) {
                return Double.parseDouble(raw);
            }
            return Long.parseLong(raw);
        }

        private void skipWhitespace() {
            while (pos < input.length() && Character.isWhitespace(input.charAt(pos))) {
                pos++;
            }
        }

        private boolean peek(char ch) {
            return pos < input.length() && input.charAt(pos) == ch;
        }

        private void expect(char ch) {
            if (!peek(ch)) {
                throw new IllegalArgumentException("expected '" + ch + "' at " + pos);
            }
            pos++;
        }
    }
}
