package omnivm;

/**
 * OmniVM - Cross-runtime bridge for Java.
 *
 * Provides static methods for calling other runtimes (Python, JavaScript, Ruby),
 * sharing typed values via callTyped(), and exchanging binary buffers.
 *
 * Native methods are registered by Go via JNI RegisterNatives during initialization.
 */
public class OmniVM {

    /**
     * Call another runtime to evaluate an expression.
     *
     * @param runtime target runtime name ("python", "javascript", "ruby", "java")
     * @param code    expression to evaluate
     * @return result string from the target runtime
     * @throws RuntimeException if the target runtime returns an error
     */
    public static String call(String runtime, String code) {
        return nativeCall(runtime, code);
    }

    /**
     * Call a function in another runtime with typed arguments.
     * Returns a typed result (Integer, Long, Double, Boolean, String, byte[], or null).
     *
     * @param runtime  target runtime name
     * @param funcName function name to call
     * @param args     typed arguments (Integer, Long, Double, Boolean, String, byte[])
     * @return typed result from the target runtime
     * @throws RuntimeException if the target runtime returns an error
     */
    public static Object callTyped(String runtime, String funcName, Object... args) {
        return nativeCallTyped(runtime, funcName, args);
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

    // Native bridge methods — implemented in Go via JNI RegisterNatives.
    public static native String nativeCall(String runtime, String code);
    public static native byte[] nativeGetBuffer(String name);
    public static native int nativeGetBufferDtype(String name);
    public static native void nativeSetBuffer(String name, byte[] data, int dtype);
    public static native void nativeReleaseBuffer(String name);
    public static native Object nativeCallTyped(String runtime, String funcName, Object[] args);

    // Capture storage for manifest executor
    private static java.util.Map<String, String> captures = new java.util.HashMap<>();

    /**
     * Set a capture value (called by manifest executor before Java code runs).
     */
    public static void setCapture(String name, String jsonValue) {
        captures.put(name, jsonValue);
    }

    /**
     * Get a capture value from the manifest executor.
     */
    public static String getCapture(String name) {
        return captures.get(name);
    }

    /**
     * Clear all captures.
     */
    public static void clearCaptures() {
        captures.clear();
    }
}
