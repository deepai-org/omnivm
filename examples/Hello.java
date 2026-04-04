public class Hello {
    public static void main(String[] args) {
        System.out.println("Hello from Java!");
        if (args.length > 0) {
            System.out.println("Args: " + String.join(", ", args));
        }
    }
}
