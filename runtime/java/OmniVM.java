package omnivm;

/**
 * OmniVM - Cross-runtime bridge for Java.
 *
 * Provides a static call() method that routes to other runtimes
 * (Python, JavaScript, Ruby) via the native bridge.
 *
 * The native method is registered by Go via JNI RegisterNatives
 * during initialization.
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
     * Native bridge method — implemented in Go via JNI RegisterNatives.
     */
    public static native String nativeCall(String runtime, String code);

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
