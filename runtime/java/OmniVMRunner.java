package omnivm;

import javax.tools.*;
import java.io.*;
import java.net.*;
import java.nio.charset.StandardCharsets;
import java.lang.reflect.Method;
import java.util.*;
import java.util.regex.*;

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
 * - Classpath extensions for Maven JARs
 * - System.out/System.err capture
 */
public class OmniVMRunner {

    private static final String WRAPPER_CLASS = "OmniVMUserCode";
    private static String classpathDir = "/omnivm/libs";

    /**
     * Execute Java source code. Entry point called from JNI.
     *
     * @param code Java source code (full class or snippet)
     * @return captured stdout output, or error prefixed with "JavaError: "
     */
    public static String execute(String code) {
        // Capture stdout and stderr
        ByteArrayOutputStream baos = new ByteArrayOutputStream();
        PrintStream capturedOut = new PrintStream(baos, true, StandardCharsets.UTF_8);
        PrintStream oldOut = System.out;
        PrintStream oldErr = System.err;

        try {
            System.setOut(capturedOut);
            System.setErr(capturedOut);

            // Determine if this is a full class or a snippet
            String className;
            String fullSource;

            if (isFullClass(code)) {
                className = extractClassName(code);
                if (className == null) {
                    return "JavaError: Could not determine class name from source";
                }
                // Strip package declaration if present for simplicity
                fullSource = code.replaceFirst("^\\s*package\\s+[^;]+;\\s*", "");
                className = extractClassName(fullSource);
            } else {
                className = WRAPPER_CLASS;
                fullSource = wrapSnippet(code);
            }

            // Compile
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
     * Eval Java expression. Returns the expression's toString() value directly,
     * not stdout capture. For use by the cross-runtime bridge.
     *
     * @param code Java expression (e.g., "1 + 2", "String.valueOf(42)")
     * @return string representation of the expression value, or error prefixed with "JavaError: "
     */
    public static String eval(String code) {
        // For simple expressions, wrap in a class that returns the value
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
                return "JavaError: No Java compiler available";
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
                // Compilation failed — maybe it's a statement, not an expression.
                // Fall back to execute() which wraps in main().
                return execute(code);
            }

            InMemoryClassLoader classLoader = new InMemoryClassLoader(fileManager.getClasses());
            Class<?> clazz = classLoader.loadClass(className);
            Method run = clazz.getMethod("run");
            Object result = run.invoke(null);

            return result == null ? "null" : result.toString();

        } catch (java.lang.reflect.InvocationTargetException e) {
            Throwable cause = e.getCause();
            if (cause != null) {
                return "JavaError: " + cause.getClass().getName() + ": " + cause.getMessage();
            }
            return "JavaError: " + e.getMessage();
        } catch (Exception e) {
            return "JavaError: " + e.getMessage();
        }
    }

    /**
     * Set the classpath directory for external JARs.
     */
    public static void setClasspathDir(String dir) {
        classpathDir = dir;
    }

    private static boolean isFullClass(String code) {
        // Check if the code contains a class/interface/enum declaration
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

    private static String wrapSnippet(String code) {
        // Check if code has import statements
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

    private static String compileAndRun(String className, String source) throws Exception {
        JavaCompiler compiler = ToolProvider.getSystemJavaCompiler();
        if (compiler == null) {
            return "JavaError: No Java compiler available (JDK required, not just JRE)";
        }

        // Set up in-memory file manager
        DiagnosticCollector<JavaFileObject> diagnostics = new DiagnosticCollector<>();
        StandardJavaFileManager stdFileManager = compiler.getStandardFileManager(diagnostics, null, null);

        // Build classpath: include external JARs if present
        List<String> options = new ArrayList<>();
        String cp = buildClasspath();
        if (cp != null && !cp.isEmpty()) {
            options.add("-classpath");
            options.add(cp);
        }

        // In-memory source file
        JavaFileObject sourceFile = new InMemoryJavaSource(className, source);

        // In-memory class output
        InMemoryFileManager fileManager = new InMemoryFileManager(stdFileManager);

        // Compile
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

        // Load and run
        InMemoryClassLoader classLoader = new InMemoryClassLoader(fileManager.getClasses());
        Class<?> clazz = classLoader.loadClass(className);

        // Look for main method
        try {
            Method main = clazz.getMethod("main", String[].class);
            main.invoke(null, (Object) new String[]{});
        } catch (NoSuchMethodException e) {
            // No main method - try to find a run() method or just instantiate
            try {
                Method run = clazz.getMethod("run");
                Object instance = clazz.getDeclaredConstructor().newInstance();
                run.invoke(instance);
            } catch (NoSuchMethodException e2) {
                // Just instantiate the class (constructor might do work)
                clazz.getDeclaredConstructor().newInstance();
            }
        } catch (java.lang.reflect.InvocationTargetException e) {
            Throwable cause = e.getCause();
            if (cause != null) {
                return "JavaError: " + cause.getClass().getName() + ": " + cause.getMessage();
            }
            throw e;
        }

        return null; // Success
    }

    private static String buildClasspath() {
        File libDir = new File(classpathDir);
        if (!libDir.exists() || !libDir.isDirectory()) {
            return System.getProperty("java.class.path", ".");
        }

        StringBuilder cp = new StringBuilder(System.getProperty("java.class.path", "."));
        File[] jars = libDir.listFiles((dir, name) -> name.endsWith(".jar"));
        if (jars != null) {
            for (File jar : jars) {
                cp.append(File.pathSeparator).append(jar.getAbsolutePath());
            }
        }
        return cp.toString();
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
