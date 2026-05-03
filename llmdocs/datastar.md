### **Core Philosophy & Architecture**

- **Backend-Driven UI:** The backend is the primary source of truth. It drives frontend state and logic by sending HTML patches, signal updates, or executable scripts to the client.
- **Hypermedia Approach:** User actions trigger requests to the backend, which responds with changes to the UI, effectively determining the next set of possible user interactions.
- **Declarative Attributes:** Use data-\* attributes in HTML to handle all frontend reactivity, event handling, and communication with the backend.

---

### **Backend-to-Frontend Communication (Patching)**

- **Patching Elements:** The backend updates the DOM by sending HTML. Datastar uses a **morphing strategy**, matching element IDs to update only what has changed.
  - **Trigger**: A frontend action like \<button data-on:click="@get('/update-ui')"\>.
  - **Response**: The backend sends a text/html response. The top-level elements in the response must have IDs that match existing elements in the DOM.
  - **SSE Event**: event: datastar-patch-elements
- **Patching Signals:** The backend updates frontend reactive state (signals) directly.
  - **Trigger**: A frontend action like \<button data-on:click="@get('/update-state')"\>.
  - **Response**: The backend sends an application/json response.
  - **SSE Event**: event: datastar-patch-signals
- **Executing Scripts:** The backend can send JavaScript for the browser to execute.
  - **Trigger**: A frontend action like \<button data-on:click="@get('/run-script')"\>.
  - **Response**: The backend sends a text/javascript response.
  - **SSE Method**: Use the ExecuteScript() SDK helper or a datastar-patch-elements event containing a \<script\> tag.

---

### **Frontend Reactivity (Signals)**

- **Signals**: Reactive variables that manage frontend state. They are always prefixed with a **$** (e.g., $foo).
- **Initialization**: Define signals with data-signals="{foo: 'initial value', form: {name: ''}}". Signals can be nested.
- **Two-Way Binding**: Use data-bind on input elements for two-way data binding.
  - Example: \<input data-bind:foo /\> or \<input data-bind="foo.name" /\> creates and binds the $foo or $foo.name signal.
- **Displaying Data**: Use data-text to set an element's text content to the value of an expression.
  - Example: \<span data-text="$foo.toUpperCase()"\>\</span\>
- **Conditional Rendering/Attributes**:
  - data-show="$foo \!= ''": Shows/hides an element.
  - data-class:active="$foo \== 'bar'": Toggles a single class.
  - data-class="{active: $foo, disabled: \!$bar}": Toggles multiple classes.
  - data-attr:disabled="$foo \== ''": Sets an attribute based on an expression.
- **Computed Signals**: Create a new, read-only signal derived from other signals using data-computed.
  - Example: \<div data-computed:isValid="$name.length \> 3" data-show="$isValid"\>\</div\>
- **Event Handling**: Use data-on to execute expressions on DOM events.
  - Example: \<button data-on:click="$counter \= $counter \+ 1; $name \= ''"\>Click Me\</button\>

---

### **Attributes (data-\*)**

Datastar's data-\* attributes are the core of its frontend functionality. They are evaluated in the order they appear in the DOM, have specific casing rules, and can be aliased to prevent conflicts. They support Datastar expressions and have built-in runtime error handling.

#### **Naming Conventions**

**IMPORTANT**: Datastar attributes follow specific naming rules:

- **HTML Attributes**: Always use kebab-case (e.g., `data-bind:my-signal`, `data-class:is-active`)
- **Signal Names**: Automatically converted to camelCase (e.g., `$mySignal`, `$isActive`)
- **In Expressions/Objects**: Use camelCase (e.g., `{mySignal: 'value'}`, `$mySignal`)

**Examples:**
```html
<!-- HTML attribute in kebab-case -->
<input data-bind:selected-firmware />

<!-- Initializes signal in camelCase -->
<div data-signals="{selectedFirmware: 'default.bin'}">

<!-- References signal in camelCase -->
<button data-on:click="$selectedFirmware = 'new.bin'">
```

#### **Common Attributes**

- **data-attr**: Sets one or more HTML attributes based on an expression.
  - \<div data-attr:title="$foo"\>\</div\>
  - \<div data-attr="{title: $foo, disabled: $bar}"\>\</div\>
- **data-bind**: Creates a signal and establishes two-way data binding with an element's value.
  - \<input data-bind:foo value="bar" /\> creates `$foo`
  - \<input data-bind:my-signal /\> creates `$mySignal` (kebab-case → camelCase)
  - \<input data-bind="foo" /\> also creates `$foo` (alternative syntax)
  - **Important**: Use kebab-case in HTML attributes, they automatically convert to camelCase signals
  - Preserves the type of predefined signals (e.g., number, array).
- **data-class**: Adds or removes one or more CSS classes based on an expression.
  - \<div data-class:hidden="$foo"\>\</div\>
  - \<div data-class="{hidden: $foo, 'font-bold': $bar}"\>\</div\>
- **data-computed**: Creates a read-only signal that is derived from other signals.
  - \<div data-computed:foo="$bar \+ $baz"\>\</div\>
- **data-effect**: Executes an expression on page load and whenever any of its dependent signals change. Useful for side effects.
  - \<div data-effect="$foo \= $bar \+ $baz"\>\</div\>
- **data-ignore**: Prevents Datastar from processing an element and its descendants.
  - data-ignore-morph specifically prevents morphing of an element.
- **data-indicator**: Creates a boolean signal that is true while a fetch request is in flight.
  - \<button data-on:click="@get('/endpoint')" data-indicator:fetching\>\</button\>
- **data-on**: Attaches an event listener to an element to execute an expression. Supports numerous modifiers for debouncing, throttling, and more.
  - \<button data-on:click\_\_debounce.500ms="$foo \= ''"\>\</button\>
- **data-on-intersect**: Executes an expression when an element intersects with the viewport.
- **data-on-interval**: Executes an expression at a regular interval.
- **data-init**: Executes an expression when the element is loaded into the DOM.
- **data-ref**: Creates a signal that is a reference to the DOM element.
- **data-show**: Shows or hides an element based on an expression.
- **data-signals**: Initializes or updates one or more signals.
  - \<div data-signals:foo="1"\>\</div\>
  - \<div data-signals="{foo: {bar: 1}}"\>\</div\>
- **data-style**: Sets one or more inline CSS styles based on an expression.
- **data-text**: Sets the text content of an element based on an expression.

#### **Pro Attributes (Commercial License)**

- **data-animate**: Animates element attributes over time.
- **data-custom-validity**: Adds custom validation to form elements.
- **data-persist**: Persists signals in local or session storage.
- **data-query-string**: Syncs signals with URL query string parameters.
- **data-replace-url**: Replaces the browser's URL without a page reload.
- **data-scroll-into-view**: Scrolls an element into view.
- **data-view-transition**: Explicitly sets the view-transition-name for an element.

---

### **Actions (@)**

Actions are secure functions that can be used within Datastar expressions, prefixed with @.

#### **Frontend Actions**

- **@peek()**: Accesses a signal's value without creating a reactive dependency.
- **@setAll()**: Sets the value of all signals that match a filter.
- **@toggleAll()**: Toggles the boolean value of all signals that match a filter.

#### **Backend Actions**

These actions send requests to the backend using the Fetch API. By default, they send all signals with the request.

- **@get(uri, options)**
- **@post(uri, options)**
- **@put(uri, options)**
- **@patch(uri, options)**
- **@delete(uri, options)**

**Response Handling:** Backend actions automatically handle different Content-Type responses:

- text/event-stream: Server-Sent Events (SSE).
- text/html: HTML to be patched into the DOM.
- application/json: JSON data to patch signals.
- text/javascript: JavaScript to be executed.

**IMPORTANT - Multiple Backend Calls:**

❌ **DO NOT** make multiple backend calls from the same element - the second call will cancel the first:

```html
<!-- WRONG - Second call cancels the first -->
<div data-init="@get('/api/endpoint1');@get('/api/endpoint2')">
```

✅ **DO** use separate elements for each backend call:

```html
<!-- CORRECT - Each element makes one call -->
<div data-init="@get('/api/endpoint1')"></div>
<div data-init="@get('/api/endpoint2')"></div>
```

✅ **OR** chain calls by having the first endpoint trigger the second via ExecuteScript:

```go
func handler1(w http.ResponseWriter, r *http.Request) {
    sse := datastar.NewSSE(w, r)

    // Do work...

    // Trigger second call via script
    sse.ExecuteScript(datastar.Script(`
        datastar.get('/api/endpoint2')
    `))
}
```

**Upload Progress (Pro):** All backend actions support file upload progress monitoring with multipart/form-data over HTTPS.

---

### **Server-Side Handling (Go SDK)**

**IMPORTANT:** Never respond to Datastar requests with plain JSON. Always use Server-Sent Events (SSE) via the Datastar Go SDK.

#### **Basic Pattern:**

```go
import "github.com/starfederation/datastar-go/datastar"

func handler(w http.ResponseWriter, r *http.Request) {
    sse := datastar.NewSSE(w, r)

    // Patch signals to update frontend state
    sse.MarshalAndPatchSignals(map[string]interface{}{
        "mySignal": "new value",
        "error": "",
    })
}
```

#### **Reading Signals from Request:**

Use `datastar.ReadSignals()` to read frontend signals sent with the request:

```go
type MyRequest struct {
    DevEUI string `json:"devEui"`  // Match signal names (camelCase)
    Name   string `json:"name"`
}

func handler(w http.ResponseWriter, r *http.Request) {
    var req MyRequest
    if err := datastar.ReadSignals(r, &req); err != nil {
        sse := datastar.NewSSE(w, r)
        sse.MarshalAndPatchSignals(map[string]interface{}{
            "error": "Invalid request",
        })
        return
    }

    // Use req.DevEUI and req.Name
    // ...
}
```

**Important:**
- Struct field tags must match the signal names from the frontend (usually camelCase)
- For GET requests, signals come from URL query parameters
- For POST/PUT/PATCH requests, signals come from JSON-encoded request body
- By default, all frontend signals are sent with backend action requests

#### **Available SSE Methods:**

- **sse.MarshalAndPatchSignals(signals map[string]interface{})** - Update frontend signals
- **sse.PatchElementsTempl(component templ.Component)** - Replace DOM elements with new HTML
- **sse.ExecuteScript(script datastar.Script)** - Execute JavaScript in the browser
- **sse.Redirect(url string)** - Redirect the browser to a new URL

#### **Example - Error Handling:**

```go
func handler(w http.ResponseWriter, r *http.Request) {
    sse := datastar.NewSSE(w, r)

    // On error, patch the error signal
    if err != nil {
        sse.MarshalAndPatchSignals(map[string]interface{}{
            "error": err.Error(),
        })
        return
    }

    // On success, patch multiple signals
    sse.MarshalAndPatchSignals(map[string]interface{}{
        "data": result,
        "error": "",
        "loading": false,
    })
}
```

#### **Example - Triggering Notifications:**

```go
// Use ExecuteScript to trigger browser events
sse.ExecuteScript(datastar.Script(`
    document.dispatchEvent(new CustomEvent('basecoat:notification', {
        detail: {
            title: 'Success',
            message: 'Operation completed!',
            type: 'success'
        }
    }));
`))
```

#### **Sequential Script Execution:**

You can execute multiple scripts in sequence using `time.Sleep()` between calls. The server controls the timing through the SSE stream, which is much cleaner than nested JavaScript `setTimeout()` callbacks.

```go
func handler(w http.ResponseWriter, r *http.Request) {
    sse := datastar.NewSSE(w, r)

    // Step 1: Execute first script
    sse.ExecuteScript(`
        if (window.sendCommand) {
            window.sendCommand(';status\n');
        }
    `)

    // Wait for device to respond
    time.Sleep(500 * time.Millisecond)

    // Step 2: Execute second script with fresh data
    sse.ExecuteScript(`
        if (window.sendCommand) {
            const timestamp = Math.floor(Date.now() / 1000);
            window.sendCommand(';settime ' + timestamp + '\n');
        }
    `)

    // Wait for processing
    time.Sleep(500 * time.Millisecond)

    // Step 3: Execute final verification script
    sse.ExecuteScript(`
        if (window.sendCommand) {
            window.sendCommand(';status\n');
        }
    `)

    // Close modal and show notification
    sse.PatchElementTempl(templates.CloseModal())
    sse.PatchElementTempl(templates.Toast("success", "Done"))
}
```

**Benefits:**
- Server orchestrates client-side actions
- Much more readable than nested JavaScript callbacks
- Each script executes sequentially as SSE events arrive
- Easy to add delays and control timing

#### **Pro Actions (Commercial License)**

- **@clipboard()**: Copies text to the clipboard.
- **@fit()**: Linearly interpolates a value from one range to another.

---

### **File Uploads**

Datastar provides a streamlined approach to file uploads without requiring traditional forms.

#### **Basic File Upload Pattern:**

```html
<div data-signals="{files: [], filesMimes: [], filesNames: []}">
  <label>
    <p>Pick anything less than 1MB</p>
    <input type="file" data-bind:files multiple/>
  </label>
  <button
    class="btn-primary"
    data-on:click="$files.length && @post('/api/upload')"
    data-attr:disabled="!$files.length"
    data-indicator:uploading>
    <span data-show="!$uploading">Upload</span>
    <span data-show="$uploading">Uploading...</span>
  </button>
</div>
```

#### **Key Features:**

- **data-bind:files**: Binds file input to signals (creates `$files`, `$filesMimes`, `$filesNames`)
- **Automatic Encoding**: Files are automatically base64 encoded before sending
- **Multiple Files**: Use `multiple` attribute to allow multiple file selection
- **Loading Indicator**: Use `data-indicator:{name}` to track upload progress

#### **Backend Handling (Go):**

```go
type FileUploadRequest struct {
    Files      []string `json:"files"`       // base64 encoded
    FilesMimes []string `json:"filesMimes"`  // MIME types
    FilesNames []string `json:"filesNames"`  // Original filenames
}

func handleFileUpload(w http.ResponseWriter, r *http.Request) {
    var req FileUploadRequest
    if err := datastar.ReadSignals(r, &req); err != nil {
        sse := datastar.NewSSE(w, r)
        sse.MarshalAndPatchSignals(map[string]interface{}{
            "uploadError": "Invalid request",
        })
        return
    }

    // Decode base64 file
    fileData, err := base64.StdEncoding.DecodeString(req.Files[0])
    if err != nil {
        sse := datastar.NewSSE(w, r)
        sse.MarshalAndPatchSignals(map[string]interface{}{
            "uploadError": "Failed to decode file",
        })
        return
    }

    // Save file
    filename := req.FilesNames[0]
    err = os.WriteFile("uploads/" + filename, fileData, 0644)
    if err != nil {
        sse := datastar.NewSSE(w, r)
        sse.MarshalAndPatchSignals(map[string]interface{}{
            "uploadError": "Failed to save file",
        })
        return
    }

    // Success
    sse := datastar.NewSSE(w, r)
    sse.MarshalAndPatchSignals(map[string]interface{}{
        "uploadSuccess": "File uploaded successfully",
        "uploadError": "",
        "files": []string{},  // Clear file input
    })
}
```

#### **Important Notes:**

- Files are base64 encoded, which increases size by ~33%
- Best for small to medium files (< 10MB)
- For large files, consider traditional multipart/form-data
- File size constraints should be enforced on backend
- Clear the `$files` signal after successful upload to reset the input
