package fixtures;

import org.springframework.http.HttpMethod;
import org.springframework.web.bind.annotation.*;

// Spring MVC verb shortcuts on a class with a `@RequestMapping` prefix.
@RestController
@RequestMapping("/api/v1")
public class Server {

  @GetMapping("/users")
  public String listUsers() { return null; }

  @PostMapping(value = "/users")
  public String createUser() { return null; }

  @PutMapping(value = "/users/{id}", consumes = "application/json")
  public String updateUser() { return null; }

  @DeleteMapping("/users/{id}")
  public void deleteUser() {}

  @PatchMapping(path = {"/users/{id}", "/people/{id}"})
  public void patchUser() {}

  // Spring `@RequestMapping` with explicit method.
  @RequestMapping(value = "/legacy", method = RequestMethod.POST)
  public void legacy() {}

  // Multi-method `@RequestMapping`.
  @RequestMapping(value = "/multi", method = {RequestMethod.GET, RequestMethod.HEAD})
  public void multi() {}
}

// JAX-RS resource: class-level `@Path` plus marker verb annotations.
@javax.ws.rs.Path("/jaxrs")
class JaxRsResource {
  @javax.ws.rs.GET
  public String list() { return null; }

  @javax.ws.rs.POST
  @javax.ws.rs.Path("/{id}")
  public void update() {}
}

// Spring `@FeignClient` interface — its annotated methods are
// outbound calls, *not* server routes.
@FeignClient(name = "kosmos", url = "https://kosmos.example.com")
interface KosmosClient {
  @GetMapping("/test/candidates")
  String getCandidates();

  @PostMapping(value = "/match", produces = "application/json")
  String match();
}
