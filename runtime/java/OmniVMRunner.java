package omnivm;

import javax.tools.*;
import java.io.*;
import java.net.*;
import java.nio.charset.StandardCharsets;
import java.nio.file.*;
import java.lang.reflect.Field;
import java.lang.reflect.Method;
import java.util.*;
import java.util.jar.*;
import java.util.regex.*;
import java.util.stream.*;

/**
 * OmniVMRunner - In-memory Java compilation and execution engine.
 *
 * Called from Go via JNI. Compiles Java source code using the
 * javax.tools.JavaCompiler (requires JDK, not just JRE), loads the
 * resulting bytecode via a custom classloader, and executes it.
 *
 * Supports:
 * - Full Java classes with main() methods
 * - Code snippets (auto-wrapped in a class)
 * - File execution (.java, .class, .jar)
 * - Classpath auto-detection (Maven, Gradle, lib/)
 * - System.out/System.err capture (REPL) or passthrough (file execution)
 * - Cross-runtime bridge via OmniVM.call()
 */
public class OmniVMRunner {

    private static final String WRAPPER_CLASS = "OmniVMUserCode";
    private static String classpathDir = "/omnivm/libs";
    private static volatile String sharedClasspath = null;
    private static volatile URLClassLoader sharedClasspathLoader = null;
    private static final File persistentClassDir =
        new File(System.getProperty("java.io.tmpdir"), "omnivm-java-classes");

    private static final class FormattedJavaError extends RuntimeException {
        private final String formatted;

        FormattedJavaError(String formatted) {
            super(formatted);
            this.formatted = formatted;
        }
    }

    // ---- File Execution (called from JNI for "omnivm run") ----
    // stdout/stderr are NOT redirected — they go to the real process streams.
    // Returns: "0" for success, "N" for System.exit(N), or "JavaError: ..." on failure.

    /**
     * Execute a file (.java, .class, or .jar) with arguments.
     * @param path     absolute path to the file
     * @param argsJoined arguments joined with \0
     * @return exit code as string, or "JavaError: ..." on failure
     */
    public static String executeFile(String path, String argsJoined) {
        String[] args = (argsJoined == null || argsJoined.isEmpty())
            ? new String[0]
            : argsJoined.split("\n", -1);

        try {
            if (path.endsWith(".jar")) {
                return runJar(path, args);
            } else if (path.endsWith(".class")) {
                return runClassFile(path, args);
            } else if (path.endsWith(".java")) {
                return runJavaFile(path, args);
            } else {
                return "JavaError: Unsupported file type: " + path;
            }
        } catch (java.lang.reflect.InvocationTargetException e) {
            Throwable cause = e.getCause();
            StringWriter sw = new StringWriter();
            (cause != null ? cause : e).printStackTrace(new PrintWriter(sw));
            return "JavaError: " + sw.toString();
        } catch (Exception e) {
            StringWriter sw = new StringWriter();
            e.printStackTrace(new PrintWriter(sw));
            return "JavaError: " + sw.toString();
        }
    }

    // ---- .java file execution ----

    private static String runJavaFile(String path, String[] args) throws Exception {
        File srcFile = new File(path).getAbsoluteFile();
        String source = new String(Files.readAllBytes(srcFile.toPath()), StandardCharsets.UTF_8);

        String className = extractClassName(source);
        if (className == null) {
            return "JavaError: Could not find class name in " + path;
        }
        String packageName = extractPackageName(source);
        String fqcn = packageName != null ? packageName + "." + className : className;

        // Build classpath from project structure
        File srcDir = srcFile.getParentFile();
        File projectRoot = findProjectRoot(srcDir);
        String cp = buildFileClasspath(projectRoot, srcDir);

        // Compile: use sourcepath so multi-file projects resolve automatically
        JavaCompiler compiler = ToolProvider.getSystemJavaCompiler();
        if (compiler == null) {
            return "JavaError: No Java compiler available (JDK required, not just JRE)";
        }

        DiagnosticCollector<JavaFileObject> diagnostics = new DiagnosticCollector<>();
        StandardJavaFileManager stdFileManager = compiler.getStandardFileManager(diagnostics, null, null);

        // Determine source root for package-relative compilation.
        // If the file has "package com.example;" and lives in src/com/example/App.java,
        // the source root is src/.
        File sourceRoot = resolveSourceRoot(srcDir, packageName);

        List<String> options = new ArrayList<>();
        options.add("-classpath");
        options.add(cp);
        options.add("-sourcepath");
        options.add(sourceRoot.getAbsolutePath());

        // Compile to an in-memory file manager
        JavaFileObject sourceFile = new InMemoryJavaSource(fqcn, source);
        InMemoryFileManager fileManager = new InMemoryFileManager(stdFileManager);

        JavaCompiler.CompilationTask task = compiler.getTask(
            null, fileManager, diagnostics, options, null,
            Collections.singletonList(sourceFile)
        );

        boolean success = task.call();
        if (!success) {
            StringBuilder errors = new StringBuilder("JavaError: Compilation failed:\n");
            for (Diagnostic<? extends JavaFileObject> d : diagnostics.getDiagnostics()) {
                if (d.getKind() == Diagnostic.Kind.ERROR) {
                    errors.append(String.format("  %s:%d: %s\n",
                        srcFile.getName(), d.getLineNumber(), d.getMessage(null)));
                }
            }
            return errors.toString().trim();
        }

        // Build a classloader that chains: in-memory classes -> file classpath
        URL[] cpUrls = classpathToURLs(cp);
        URLClassLoader parentLoader = new URLClassLoader(cpUrls, OmniVMRunner.class.getClassLoader());
        InMemoryClassLoader classLoader = new InMemoryClassLoader(fileManager.getClasses(), parentLoader);

        return invokeMain(classLoader, fqcn, args);
    }

    // ---- .class file execution ----

    private static String runClassFile(String path, String[] args) throws Exception {
        File classFile = new File(path).getAbsoluteFile();
        File dir = classFile.getParentFile();

        // Derive class name: strip .class extension
        String simpleName = classFile.getName().replace(".class", "");

        // Try to detect package from directory structure by reading the class bytes.
        // Simpler approach: add the directory to classpath and try loading by simple name.
        // If that fails, walk up looking for a reasonable root.
        File projectRoot = findProjectRoot(dir);
        String cp = buildFileClasspath(projectRoot, dir);

        URL[] cpUrls = classpathToURLs(cp);
        URLClassLoader classLoader = new URLClassLoader(cpUrls, OmniVMRunner.class.getClassLoader());

        // Try simple name first, then try to figure out FQCN from directory structure
        String fqcn = inferFQCN(dir, simpleName, projectRoot);

        return invokeMain(classLoader, fqcn, args);
    }

    // ---- .jar file execution ----

    private static String runJar(String path, String[] args) throws Exception {
        File jarFile = new File(path).getAbsoluteFile();

        // Read Main-Class from manifest
        String mainClass = null;
        try (JarFile jar = new JarFile(jarFile)) {
            Manifest manifest = jar.getManifest();
            if (manifest != null) {
                mainClass = manifest.getMainAttributes().getValue("Main-Class");
            }
        }
        if (mainClass == null) {
            return "JavaError: No Main-Class in manifest of " + path;
        }

        // Build classpath: the jar itself + its Class-Path entries + project libs
        File projectRoot = findProjectRoot(jarFile.getParentFile());
        StringBuilder cp = new StringBuilder(jarFile.getAbsolutePath());

        // Add Class-Path from manifest
        try (JarFile jar = new JarFile(jarFile)) {
            String cpAttr = jar.getManifest().getMainAttributes().getValue("Class-Path");
            if (cpAttr != null) {
                for (String entry : cpAttr.split("\\s+")) {
                    File resolved = new File(jarFile.getParentFile(), entry);
                    if (resolved.exists()) {
                        cp.append(File.pathSeparator).append(resolved.getAbsolutePath());
                    }
                }
            }
        }

        // Add standard library paths
        cp.append(File.pathSeparator).append(buildFileClasspath(projectRoot, jarFile.getParentFile()));

        URL[] cpUrls = classpathToURLs(cp.toString());
        URLClassLoader classLoader = new URLClassLoader(cpUrls, OmniVMRunner.class.getClassLoader());

        return invokeMain(classLoader, mainClass, args);
    }

    // ---- Shared: invoke main() with System.exit() interception ----

    private static String invokeMain(ClassLoader classLoader, String fqcn, String[] args) throws Exception {
        Class<?> clazz;
        try {
            clazz = classLoader.loadClass(fqcn);
        } catch (ClassNotFoundException e) {
            return "JavaError: Class not found: " + fqcn;
        }

        Method main;
        try {
            main = clazz.getMethod("main", String[].class);
        } catch (NoSuchMethodException e) {
            return "JavaError: No main(String[]) method in " + fqcn;
        }

        main.invoke(null, (Object) args);
        return "0";
    }

    // ---- Classpath auto-detection ----

    /**
     * Build classpath for file execution by scanning the project structure.
     * Looks for Maven, Gradle, and manual lib directories.
     */
    private static String buildFileClasspath(File projectRoot, File srcDir) {
        List<String> entries = new ArrayList<>();

        entries.add(persistentClassDir.getAbsolutePath());

        // Source directory itself (for .class files alongside .java)
        entries.add(srcDir.getAbsolutePath());

        if (projectRoot != null) {
            // Maven: target/classes, target/dependency/*.jar, target/*.jar
            addDirIfExists(entries, projectRoot, "target/classes");
            addJarsFrom(entries, projectRoot, "target/dependency");
            addJarsFrom(entries, projectRoot, "target");

            // Gradle: build/classes/java/main, build/libs/*.jar
            addDirIfExists(entries, projectRoot, "build/classes/java/main");
            addJarsFrom(entries, projectRoot, "build/libs");

            // Common: lib/*.jar, libs/*.jar
            addJarsFrom(entries, projectRoot, "lib");
            addJarsFrom(entries, projectRoot, "libs");
        }

        // OmniVM default libs
        addJarsFrom(entries, new File("/omnivm"), "libs");

        // System classpath (includes OmniVMRunner/OmniVM classes)
        String sysCp = System.getProperty("java.class.path", "");
        if (!sysCp.isEmpty()) {
            entries.add(sysCp);
        }

        return String.join(File.pathSeparator, entries);
    }

    private static void addDirIfExists(List<String> entries, File root, String sub) {
        File dir = new File(root, sub);
        if (dir.isDirectory()) {
            entries.add(dir.getAbsolutePath());
        }
    }

    private static void addJarsFrom(List<String> entries, File root, String sub) {
        File dir = new File(root, sub);
        if (!dir.isDirectory()) return;
        File[] jars = dir.listFiles((d, name) -> name.endsWith(".jar"));
        if (jars != null) {
            for (File jar : jars) {
                entries.add(jar.getAbsolutePath());
            }
        }
    }

    /**
     * Walk up from srcDir looking for project markers (pom.xml, build.gradle, etc).
     * Returns the project root, or srcDir if no marker found.
     */
    private static File findProjectRoot(File dir) {
        File current = dir;
        for (int i = 0; i < 10 && current != null; i++) {
            if (new File(current, "pom.xml").exists() ||
                new File(current, "build.gradle").exists() ||
                new File(current, "build.gradle.kts").exists() ||
                new File(current, "lib").isDirectory() ||
                new File(current, "libs").isDirectory()) {
                return current;
            }
            current = current.getParentFile();
        }
        return dir;
    }

    /**
     * Given that a source file is in srcDir and declares packageName,
     * resolve the source root by stripping package-matching directories.
     * E.g., srcDir=/project/src/com/example, package=com.example → /project/src
     */
    private static File resolveSourceRoot(File srcDir, String packageName) {
        if (packageName == null || packageName.isEmpty()) {
            return srcDir;
        }
        String[] parts = packageName.split("\\.");
        File root = srcDir;
        // Walk up, matching package components from the end
        for (int i = parts.length - 1; i >= 0; i--) {
            if (root.getName().equals(parts[i])) {
                root = root.getParentFile();
            } else {
                // Directory structure doesn't match package — just use srcDir
                return srcDir;
            }
        }
        return root;
    }

    /**
     * Try to infer FQCN for a .class file by checking directory structure against
     * known class output directories (target/classes, build/classes/java/main).
     */
    private static String inferFQCN(File classDir, String simpleName, File projectRoot) {
        // Check if the class lives under a known output directory
        String path = classDir.getAbsolutePath();
        String[] roots = {
            projectRoot + "/target/classes",
            projectRoot + "/build/classes/java/main",
        };
        for (String root : roots) {
            if (path.startsWith(root) && path.length() > root.length()) {
                String relative = path.substring(root.length() + 1);
                return relative.replace(File.separatorChar, '.') + "." + simpleName;
            }
        }
        return simpleName;
    }

    private static URL[] classpathToURLs(String cp) {
        return Arrays.stream(cp.split(File.pathSeparator))
            .filter(s -> !s.isEmpty())
            .map(s -> {
                try {
                    return new File(s).toURI().toURL();
                } catch (Exception e) {
                    return null;
                }
            })
            .filter(Objects::nonNull)
            .toArray(URL[]::new);
    }

    private static synchronized ClassLoader sharedClasspathLoader(String cp) {
        String key = cp == null ? "" : cp;
        if (sharedClasspathLoader == null || !key.equals(sharedClasspath)) {
            sharedClasspath = key;
            sharedClasspathLoader = new URLClassLoader(classpathToURLs(key), OmniVMRunner.class.getClassLoader());
        }
        return sharedClasspathLoader;
    }

    private static synchronized void invalidateSharedClasspathLoader() {
        sharedClasspath = null;
        sharedClasspathLoader = null;
    }

    // ---- REPL execution (with stdout capture) ----

    /**
     * Execute Java source code. Entry point called from JNI for REPL/inline mode.
     * Captures stdout/stderr and returns the output.
     *
     * @param code Java source code (full class or snippet)
     * @return captured stdout output, or error prefixed with "JavaError: "
     */
    public static String execute(String code) {
        ByteArrayOutputStream baos = new ByteArrayOutputStream();
        PrintStream capturedOut = new PrintStream(baos, true, StandardCharsets.UTF_8);
        PrintStream oldOut = System.out;
        PrintStream oldErr = System.err;

        try {
            System.setOut(capturedOut);
            System.setErr(capturedOut);

            String className;
            String fullSource;

            if (isFullClass(code)) {
                className = extractClassName(code);
                if (className == null) {
                    return "JavaError: Could not determine class name from source";
                }
                String pkg = extractPackageName(code);
                if (pkg != null) {
                    // Keep source intact — compile with package
                    fullSource = code;
                    className = pkg + "." + className;
                } else {
                    fullSource = code;
                }
            } else {
                className = WRAPPER_CLASS;
                fullSource = wrapSnippet(code);
            }

            String compileError = compileAndRun(className, fullSource);
            if (compileError != null) {
                return compileError;
            }

            capturedOut.flush();
            return baos.toString(StandardCharsets.UTF_8);

        } catch (Exception e) {
            StringWriter sw = new StringWriter();
            e.printStackTrace(new PrintWriter(sw));
            return "JavaError: " + e.getMessage() + "\n" + sw.toString();
        } finally {
            System.setOut(oldOut);
            System.setErr(oldErr);
        }
    }

    /**
     * Eval Java expression. Returns the expression's toString() value directly.
     * For use by the cross-runtime bridge.
     */
    public static String eval(String code) {
        Object result = evalObject(code);
        if (result instanceof FormattedJavaError e) {
            return "JavaError: " + e.formatted;
        }
        if (result instanceof Throwable t) {
            return "JavaError: " + formatThrowable(t);
        }
        return result == null ? "null" : result.toString();
    }

    /**
     * Eval Java expression and return the raw object to the embedding runtime.
     * User-facing eval still stringifies; this exists for automatic boundary
     * inference/export paths that need to inspect the Java object shape.
     */
    public static Object evalObject(String code) {
        String className = "OmniVMEval";
        String source =
            "public class " + className + " {\n" +
            "    public static Object run() throws Exception {\n" +
            "        return " + code + ";\n" +
            "    }\n" +
            "}\n";

        try {
            JavaCompiler compiler = ToolProvider.getSystemJavaCompiler();
            if (compiler == null) {
                return new RuntimeException("No Java compiler available");
            }

            DiagnosticCollector<JavaFileObject> diagnostics = new DiagnosticCollector<>();
            StandardJavaFileManager stdFileManager = compiler.getStandardFileManager(diagnostics, null, null);

            List<String> options = new ArrayList<>();
            String cp = buildClasspath();
            if (cp != null && !cp.isEmpty()) {
                options.add("-classpath");
                options.add(cp);
            }

            JavaFileObject sourceFile = new InMemoryJavaSource(className, source);
            InMemoryFileManager fileManager = new InMemoryFileManager(stdFileManager);

            JavaCompiler.CompilationTask task = compiler.getTask(
                null, fileManager, diagnostics, options, null,
                Collections.singletonList(sourceFile)
            );

            boolean success = task.call();
            if (!success) {
                String executed = execute(code);
                if (executed != null && executed.startsWith("JavaError: ")) {
                    return new FormattedJavaError(executed.substring("JavaError: ".length()));
                }
                return executed;
            }

            ClassLoader parentLoader = sharedClasspathLoader(cp);
            InMemoryClassLoader classLoader = new InMemoryClassLoader(fileManager.getClasses(), parentLoader);
            Class<?> clazz = classLoader.loadClass(className);
            Method run = clazz.getMethod("run");
            return run.invoke(null);

        } catch (java.lang.reflect.InvocationTargetException e) {
            Throwable cause = e.getCause();
            if (cause != null) {
                return cause;
            }
            return e;
        } catch (Exception e) {
            return e;
        }
    }

    public static String interruptibleSleep(long millis) throws InterruptedException {
        Thread.sleep(millis);
        return "awake";
    }

    public static void setClasspathDir(String dir) {
        classpathDir = dir;
    }

    // ---- Source analysis ----

    private static boolean isFullClass(String code) {
        return code.matches("(?s).*\\b(public\\s+)?(class|interface|enum|record)\\s+\\w+.*");
    }

    private static String extractClassName(String code) {
        Pattern p = Pattern.compile("\\b(?:public\\s+)?(?:class|interface|enum|record)\\s+(\\w+)");
        Matcher m = p.matcher(code);
        if (m.find()) {
            return m.group(1);
        }
        return null;
    }

    private static String extractPackageName(String code) {
        Pattern p = Pattern.compile("^\\s*package\\s+([\\w.]+)\\s*;", Pattern.MULTILINE);
        Matcher m = p.matcher(code);
        if (m.find()) {
            return m.group(1);
        }
        return null;
    }

    private static String wrapSnippet(String code) {
        StringBuilder imports = new StringBuilder();
        StringBuilder body = new StringBuilder();

        for (String line : code.split("\n")) {
            String trimmed = line.trim();
            if (trimmed.startsWith("import ")) {
                imports.append(line).append("\n");
            } else {
                body.append(line).append("\n");
            }
        }

        return imports.toString() +
            "public class " + WRAPPER_CLASS + " {\n" +
            "    public static void main(String[] args) throws Exception {\n" +
            "        " + body.toString() + "\n" +
            "    }\n" +
            "}\n";
    }

    // ---- REPL compilation (in-memory, with stdout capture) ----

    private static String compileAndRun(String className, String source) throws Exception {
        JavaCompiler compiler = ToolProvider.getSystemJavaCompiler();
        if (compiler == null) {
            return "JavaError: No Java compiler available (JDK required, not just JRE)";
        }

        DiagnosticCollector<JavaFileObject> diagnostics = new DiagnosticCollector<>();
        StandardJavaFileManager stdFileManager = compiler.getStandardFileManager(diagnostics, null, null);

        List<String> options = new ArrayList<>();
        String cp = buildClasspath();
        if (cp != null && !cp.isEmpty()) {
            options.add("-classpath");
            options.add(cp);
        }

        // For FQCN like "com.example.Foo", the source file URI needs the path form
        String sourcePath = className.replace('.', '/');
        JavaFileObject sourceFile = new InMemoryJavaSource(sourcePath, source);
        InMemoryFileManager fileManager = new InMemoryFileManager(stdFileManager);

        JavaCompiler.CompilationTask task = compiler.getTask(
            null, fileManager, diagnostics, options, null,
            Collections.singletonList(sourceFile)
        );

        boolean success = task.call();
        if (!success) {
            StringBuilder errors = new StringBuilder("JavaError: Compilation failed:\n");
            for (Diagnostic<? extends JavaFileObject> d : diagnostics.getDiagnostics()) {
                errors.append(String.format("  Line %d: %s\n",
                    d.getLineNumber(), d.getMessage(null)));
            }
            return errors.toString();
        }

        persistCompiledClasses(className, fileManager.getClasses());

        ClassLoader parentLoader = sharedClasspathLoader(cp);
        InMemoryClassLoader classLoader = new InMemoryClassLoader(fileManager.getClasses(), parentLoader);
        Class<?> clazz = classLoader.loadClass(className);

        try {
            Method main = clazz.getMethod("main", String[].class);
            main.invoke(null, (Object) new String[]{});
        } catch (NoSuchMethodException e) {
            try {
                Method run = clazz.getMethod("run");
                Object instance = clazz.getDeclaredConstructor().newInstance();
                run.invoke(instance);
            } catch (NoSuchMethodException e2) {
                clazz.getDeclaredConstructor().newInstance();
            }
        } catch (java.lang.reflect.InvocationTargetException e) {
            Throwable cause = e.getCause();
            if (cause != null) {
                return "JavaError: " + formatThrowable(cause);
            }
            throw e;
        }

        return null; // Success
    }

    private static String formatThrowable(Throwable throwable) {
        if (throwable instanceof OmniVM.RuntimeError e) {
            StringBuilder out = new StringBuilder();
            if (e.getRuntime() != null && !e.getRuntime().isEmpty()) {
                out.append(e.getRuntime()).append(": ");
            }
            if (e.getType() != null && !e.getType().isEmpty()) {
                out.append(e.getType()).append(": ");
            }
            out.append(e.getMessage() == null ? "" : e.getMessage());
            if (e.getTraceback() != null && !e.getTraceback().isEmpty()) {
                out.append('\n').append(e.getTraceback());
            }
            for (Map<String, String> cause : e.getCauseChain()) {
                out.append("\nCaused by: ");
                String type = cause.get("type");
                if (type != null && !type.isEmpty()) {
                    out.append(type).append(": ");
                }
                out.append(cause.getOrDefault("message", ""));
            }
            if (e.getDetailsJson() != null && !e.getDetailsJson().isEmpty()) {
                out.append("\nDetails: ").append(e.getDetailsJson());
            }
            if (e.getOriginalErrorHandle() != null && !e.getOriginalErrorHandle().isEmpty()) {
                out.append("\nOriginal error handle: ").append(e.getOriginalErrorHandle());
            }
            return out.toString().trim();
        }
        StringWriter sw = new StringWriter();
        throwable.printStackTrace(new PrintWriter(sw));
        String text = sw.toString().trim();
        String handle = originalErrorHandle(throwable);
        if (handle != null && !handle.isEmpty()) {
            text += "\nOriginal error handle: " + handle;
        }
        return text;
    }

    private static String originalErrorHandle(Throwable throwable) {
        String handle = originalErrorHandleFromMethod(throwable, "getOriginalErrorHandle");
        if (handle != null && !handle.isEmpty()) {
            return handle;
        }
        handle = originalErrorHandleFromMethod(throwable, "originalErrorHandle");
        if (handle != null && !handle.isEmpty()) {
            return handle;
        }
        handle = originalErrorHandleFromField(throwable, "originalErrorHandle");
        if (handle != null && !handle.isEmpty()) {
            return handle;
        }
        return originalErrorHandleFromField(throwable, "original_error_handle");
    }

    private static String originalErrorHandleFromMethod(Throwable throwable, String name) {
        try {
            Method method = throwable.getClass().getMethod(name);
            if (method.getParameterCount() != 0) {
                return "";
            }
            Object value = method.invoke(throwable);
            return value == null ? "" : String.valueOf(value);
        } catch (ReflectiveOperationException | SecurityException ignored) {
            return "";
        }
    }

    private static String originalErrorHandleFromField(Throwable throwable, String name) {
        for (Class<?> current = throwable.getClass(); current != null; current = current.getSuperclass()) {
            try {
                Field field = current.getDeclaredField(name);
                field.setAccessible(true);
                Object value = field.get(throwable);
                return value == null ? "" : String.valueOf(value);
            } catch (ReflectiveOperationException | RuntimeException ignored) {
                // Try the next superclass.
            }
        }
        return "";
    }

    private static void persistCompiledClasses(String entryClassName, List<InMemoryClassFile> classes) {
        if (WRAPPER_CLASS.equals(entryClassName) || "OmniVMEval".equals(entryClassName)) {
            return;
        }
        boolean wrote = false;
        for (InMemoryClassFile cf : classes) {
            try {
                File out = new File(persistentClassDir, cf.getClassName().replace('.', File.separatorChar) + ".class");
                File parent = out.getParentFile();
                if (parent != null) {
                    parent.mkdirs();
                }
                Files.write(out.toPath(), cf.getBytes());
                wrote = true;
            } catch (IOException ignored) {
            }
        }
        if (wrote) {
            invalidateSharedClasspathLoader();
        }
    }

    /** Classpath for REPL mode — system cp + /omnivm/libs */
    private static String buildClasspath() {
        File libDir = new File(classpathDir);
        if (!libDir.exists() || !libDir.isDirectory()) {
            return persistentClassDir.getAbsolutePath() + File.pathSeparator + System.getProperty("java.class.path", ".");
        }

        StringBuilder cp = new StringBuilder(persistentClassDir.getAbsolutePath());
        cp.append(File.pathSeparator).append(System.getProperty("java.class.path", "."));
        appendJars(cp, libDir);
        appendJars(cp, new File("/omnivm/libs"));
        return cp.toString();
    }

    private static void appendJars(StringBuilder cp, File libDir) {
        if (!libDir.exists() || !libDir.isDirectory()) {
            return;
        }
        File[] jars = libDir.listFiles((dir, name) -> name.endsWith(".jar"));
        if (jars != null) {
            for (File jar : jars) {
                cp.append(File.pathSeparator).append(jar.getAbsolutePath());
            }
        }
    }

    // ---- In-memory compilation infrastructure ----

    static class InMemoryJavaSource extends SimpleJavaFileObject {
        private final String code;

        InMemoryJavaSource(String className, String code) {
            super(URI.create("string:///" + className.replace('.', '/') + Kind.SOURCE.extension),
                  Kind.SOURCE);
            this.code = code;
        }

        @Override
        public CharSequence getCharContent(boolean ignoreEncodingErrors) {
            return code;
        }
    }

    static class InMemoryClassFile extends SimpleJavaFileObject {
        private final ByteArrayOutputStream bos = new ByteArrayOutputStream();
        private final String className;

        InMemoryClassFile(String className) {
            super(URI.create("mem:///" + className.replace('.', '/') + Kind.CLASS.extension),
                  Kind.CLASS);
            this.className = className;
        }

        @Override
        public OutputStream openOutputStream() {
            return bos;
        }

        byte[] getBytes() {
            return bos.toByteArray();
        }

        String getClassName() {
            return className;
        }
    }

    static class InMemoryFileManager extends ForwardingJavaFileManager<StandardJavaFileManager> {
        private final List<InMemoryClassFile> classes = new ArrayList<>();

        InMemoryFileManager(StandardJavaFileManager fileManager) {
            super(fileManager);
        }

        @Override
        public JavaFileObject getJavaFileForOutput(Location location, String className,
                JavaFileObject.Kind kind, FileObject sibling) {
            InMemoryClassFile classFile = new InMemoryClassFile(className);
            classes.add(classFile);
            return classFile;
        }

        List<InMemoryClassFile> getClasses() {
            return classes;
        }
    }

    static class InMemoryClassLoader extends ClassLoader {
        private final Map<String, byte[]> classData = new HashMap<>();

        InMemoryClassLoader(List<InMemoryClassFile> classes) {
            for (InMemoryClassFile cf : classes) {
                classData.put(cf.getClassName(), cf.getBytes());
            }
        }

        InMemoryClassLoader(List<InMemoryClassFile> classes, ClassLoader parent) {
            super(parent);
            for (InMemoryClassFile cf : classes) {
                classData.put(cf.getClassName(), cf.getBytes());
            }
        }

        @Override
        protected Class<?> findClass(String name) throws ClassNotFoundException {
            byte[] data = classData.get(name);
            if (data == null) {
                return super.findClass(name);
            }
            return defineClass(name, data, 0, data.length);
        }
    }
}
