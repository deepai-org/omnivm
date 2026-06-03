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
import java.util.Map;
import java.util.NoSuchElementException;
import java.util.Set;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.atomic.AtomicLong;

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
        if (target instanceof Map map) {
            map.put(key, value);
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

    public static Object proxyCall(Object target, Object keyValue, Object argsValue) {
        String key = proxyKey(keyValue);
        List<?> args = argsValue instanceof List<?> list ? list : Collections.emptyList();
        if (target == null) {
            return null;
        }
        if (key == null || key.isEmpty()) {
            return invokeCallableTarget(target, args);
        }
        for (java.lang.reflect.Method method : proxyMethods(target.getClass())) {
            if (!method.getName().equals(key) || method.getParameterCount() != args.size()) {
                continue;
            }
            try {
                Object[] converted = new Object[args.size()];
                Class<?>[] types = method.getParameterTypes();
                for (int i = 0; i < args.size(); i++) {
                    converted[i] = coerceArg(args.get(i), types[i]);
                }
                return invokeProxyMethod(method, target, converted);
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
        try {
            Object[] converted = new Object[args.size()];
            Class<?>[] types = method.getParameterTypes();
            for (int i = 0; i < args.size(); i++) {
                converted[i] = coerceArg(args.get(i), types[i]);
            }
            try {
                method.setAccessible(true);
            } catch (Throwable ignored) {
            }
            return method.invoke(target, converted);
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
        return method.invoke(target, args);
    }

    private static void makeAccessible(java.lang.reflect.AccessibleObject object) {
        try {
            object.setAccessible(true);
        } catch (RuntimeException ignored) {
        }
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
    public static final class HandleProxy extends AbstractMap<String, Object> {
        private static final Set<String> chattyProxyWarned = Collections.newSetFromMap(new java.util.concurrent.ConcurrentHashMap<String, Boolean>());
        private static final int chattyProxyWarnedLimit = 4096;
        private final Map<String, Object> value;
        private final Cleaner.Cleanable cleanable;

        private HandleProxy(Map<String, Object> value) {
            this.value = value;
            if (Boolean.TRUE.equals(value.get("transfer"))) {
                adopt(value.get("id"));
            } else {
                retain(value.get("id"));
            }
            this.cleanable = captureCleaner.register(this, new FinalizerState(value.get("id")));
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
            record("property");
            return value.get("id");
        }

        public String runtime() {
            record("property");
            Object runtime = value.get("runtime");
            return runtime == null ? null : runtime.toString();
        }

        public String kind() {
            record("property");
            Object kind = value.get("kind");
            return kind == null ? null : kind.toString();
        }

        public Map<String, Object> asMap() {
            record("iterate");
            return Collections.unmodifiableMap(value);
        }

        public void releaseFromFinalizer() {
            cleanable.clean();
        }

        @Override
        public Object get(Object key) {
            if (isIndexedDescriptor() && numericIndex(key) != null) {
                return index(key);
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
            return bridgeGet(String.valueOf(key));
        }

        private boolean isIndexedDescriptor() {
            return Boolean.TRUE.equals(value.get("__omnivm_table__")) || "sequence".equals(String.valueOf(value.get("kind")));
        }

        public Object index(Object key) {
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
            record("mutation");
            Object result = bridgeOp("{\"op\":\"handle_set\",\"id\":" + jsonScalar(value.get("id")) + ",\"key\":\"" + jsonEscape(key) + "\",\"value\":" + jsonValue(encodeArg(next)) + "}");
            return Boolean.TRUE.equals(result);
        }

        public Object call(String key, Object... args) {
            record("call");
            return bridgeOp("{\"op\":\"handle_call\",\"id\":" + jsonScalar(value.get("id")) + ",\"key\":\"" + jsonEscape(key) + "\",\"args\":" + jsonArray(args) + "}");
        }

        public Object apply(Object... args) {
            record("call");
            return bridgeOp("{\"op\":\"handle_call\",\"id\":" + jsonScalar(value.get("id")) + ",\"key\":\"\",\"args\":" + jsonArray(args) + "}");
        }

        @Override
        public int size() {
            Object length = bridgeOp("{\"op\":\"handle_len\",\"id\":" + jsonScalar(value.get("id")) + "}");
            if (length instanceof Number) {
                return ((Number) length).intValue();
            }
            record("property");
            return value.size();
        }

        @Override
        @SuppressWarnings("unchecked")
        public Collection<Object> values() {
            Object values = bridgeOp("{\"op\":\"handle_iter\",\"id\":" + jsonScalar(value.get("id")) + ",\"mode\":\"values\"}");
            if (values instanceof List<?>) {
                return Collections.unmodifiableList((List<Object>) values);
            }
            return super.values();
        }

        @Override
        public Set<Entry<String, Object>> entrySet() {
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
            record("iterate");
            return Collections.unmodifiableMap(value).entrySet();
        }

        @Override
        public boolean containsKey(Object key) {
            Object contains = bridgeOp("{\"op\":\"handle_contains\",\"id\":" + jsonScalar(value.get("id")) + ",\"value\":" + jsonValue(key) + "}");
            if (contains instanceof Boolean) {
                return Boolean.TRUE.equals(contains);
            }
            record("property");
            return hasLocalValue(key);
        }

        @Override
        public String toString() {
            return value.toString();
        }

        private Map<?, ?> record(String kind) {
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
            if (!Boolean.TRUE.equals(value.get("__omnivm_resource__")) || key == null) {
                return false;
            }
            String text = String.valueOf(key);
            return "__omnivm_resource__".equals(text)
                || "id".equals(text)
                || "runtime".equals(text)
                || "kind".equals(text)
                || "closed".equals(text)
                || "transfer".equals(text)
                || "disposer".equals(text)
                || "__omnivm_materialized__".equals(text);
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
            if (Boolean.TRUE.equals(value.get("__omnivm_materialized__"))) {
                return;
            }
            Object items = bridgeOp("{\"op\":\"handle_iter\",\"id\":" + jsonScalar(value.get("id")) + ",\"mode\":\"items\",\"materialize\":true}");
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

        @SuppressWarnings("unchecked")
        private Object bridgeGet(String key) {
            Object id = value.get("id");
            if (id == null || key == null) {
                return null;
            }
            return bridgeOp("{\"op\":\"handle_get\",\"id\":" + jsonScalar(id) + ",\"key\":\"" + jsonEscape(key) + "\"}");
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

        private FinalizerState(Object id) {
            this.id = id;
        }

        @Override
        public void run() {
            if (id == null) {
                return;
            }
            try {
                OmniVM.call("__manifest", "{\"op\":\"handle_release_finalizer\",\"id\":" + jsonScalar(id) + "}");
            } catch (Throwable ignored) {
            }
        }
    }

    public static final class StreamProxy implements Iterable<Object> {
        private final Map<String, Object> value;
        private final Cleaner.Cleanable cleanable;

        private StreamProxy(Map<String, Object> value) {
            this.value = value;
            if (Boolean.TRUE.equals(value.get("transfer"))) {
                HandleProxy.adopt(value.get("id"));
            } else {
                HandleProxy.retain(value.get("id"));
            }
            this.cleanable = captureCleaner.register(this, new FinalizerState(value.get("id")));
        }

        public void releaseFromFinalizer() {
            cleanable.clean();
        }

        public boolean cancel() {
            Object id = value.get("id");
            Object result = bridgeManifestOp("{\"op\":\"stream_cancel\",\"id\":" + jsonScalar(id) + "}");
            return Boolean.TRUE.equals(result);
        }

        public List<Object> toList() {
            List<Object> out = new ArrayList<>();
            for (Object item : this) {
                out.add(item);
            }
            return out;
        }

        @Override
        public Iterator<Object> iterator() {
            return new Iterator<Object>() {
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
                    Object id = value.get("id");
                    Object result = bridgeManifestOp("{\"op\":\"stream_next\",\"id\":" + jsonScalar(id) + "}");
                    if (!(result instanceof Map<?, ?> item)) {
                        done = true;
                        return;
                    }
                    if (Boolean.TRUE.equals(item.get("done"))) {
                        done = true;
                        return;
                    }
                    next = materializeCapture(((Map<String, Object>) item).get("value"));
                }
            };
        }
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
        if ((target == boolean.class || target == Boolean.class) && value instanceof Boolean bool) {
            return bool;
        }
        if (target == String.class) {
            return String.valueOf(value);
        }
        return value;
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
