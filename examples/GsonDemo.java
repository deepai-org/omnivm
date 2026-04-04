import com.google.gson.Gson;
import com.google.gson.GsonBuilder;
import java.util.*;

public class GsonDemo {
    public static void main(String[] args) {
        Gson gson = new GsonBuilder().setPrettyPrinting().create();

        // Build a data structure
        Map<String, Object> data = new LinkedHashMap<>();
        data.put("name", "OmniVM");
        data.put("languages", Arrays.asList("Python", "JavaScript", "Java", "Ruby", "Go"));
        data.put("version", 1.0);
        data.put("embedded", true);

        // Serialize to JSON
        String json = gson.toJson(data);
        System.out.println("Serialized:");
        System.out.println(json);

        // Deserialize back
        @SuppressWarnings("unchecked")
        Map<String, Object> parsed = gson.fromJson(json, Map.class);
        System.out.println("\nDeserialized languages: " + parsed.get("languages"));

        // Handle command-line arguments
        if (args.length > 0) {
            Map<String, Object> argsMap = new LinkedHashMap<>();
            argsMap.put("args", Arrays.asList(args));
            argsMap.put("count", args.length);
            System.out.println("\nArgs as JSON: " + gson.toJson(argsMap));
        }

        System.out.println("\nGson " + Gson.class.getPackage().getImplementationVersion() + " works!");
    }
}
