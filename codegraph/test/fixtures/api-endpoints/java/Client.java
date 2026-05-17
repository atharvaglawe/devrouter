package fixtures;

import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import org.springframework.web.client.RestTemplate;
import org.springframework.web.reactive.function.client.WebClient;
import org.springframework.http.HttpMethod;
import okhttp3.OkHttpClient;
import okhttp3.Request;
import org.apache.http.client.methods.HttpGet;
import org.apache.http.client.methods.HttpPost;

class ApiClients {

  // RestTemplate
  void useRestTemplate(RestTemplate rt) throws Exception {
    rt.getForObject("/api/items", String.class);
    rt.postForEntity("/api/items", null, String.class);
    rt.exchange("/api/users", HttpMethod.PUT, null, String.class);
    rt.delete("/api/users/{id}");
  }

  // Spring WebClient
  void useWebClient(WebClient client) {
    client.get().uri("/api/health").retrieve();
    client.method(HttpMethod.POST).uri("/api/orders").retrieve();
  }

  // OkHttp
  void useOkHttp(OkHttpClient ok) throws Exception {
    Request req = new Request.Builder().url("/api/feed").get().build();
    Request req2 = new Request.Builder().url("/api/feed").post(null).build();
    ok.newCall(req).execute();
  }

  // Apache HttpClient
  void useApache() {
    HttpGet g = new HttpGet("/api/legacy");
    HttpPost p = new HttpPost("/api/submit");
  }

  // java.net.http
  void useJavaNetHttp() throws Exception {
    HttpRequest r = HttpRequest.newBuilder(URI.create("/api/data")).GET().build();
    HttpRequest r2 = HttpRequest.newBuilder().uri(URI.create("/api/upload"))
        .method("POST", HttpRequest.BodyPublishers.noBody()).build();
    HttpClient.newHttpClient().send(r, HttpResponse.BodyHandlers.ofString());
  }
}
