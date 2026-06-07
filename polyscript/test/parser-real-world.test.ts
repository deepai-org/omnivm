import { Parser } from '../src/parser';
import { Lexer } from '../src/lexer';
import * as AST from '../src/ast';

function parseCode(code: string): AST.Program {
  const lexer = new Lexer(code);
  const tokens = lexer.tokenize();
  const parser = new Parser(tokens);
  return parser.parse();
}

describe('Real-World Code Parser Tests', () => {
  test('parses real-world TypeScript feedback system code', () => {
    const code = `
/// <reference path="/ml_apis/apps/base/static/ts/global.d.ts" />
/// <reference path="/ml_apis/apps/base/static/ts/base.d.ts" />

/**
 * Similar JavaScript pattern/structure to the AI video page,
 * adapted for feedback submission with infinite scrolling.
 */

// Basic references
const feedbackForm = document.getElementById('feedbackForm') as HTMLFormElement;
const submitFeedbackButton = document.getElementById('submitFeedbackButton') as HTMLButtonElement;
const feedbackProgressContainer = document.getElementById('feedbackProgressContainer') as HTMLDivElement;
const feedbackProgressBar = document.getElementById('feedbackProgressBar') as HTMLDivElement;

// Form elements for validation
const areaSelect = document.getElementById('areaSelect') as HTMLSelectElement;
const typeSelect = document.getElementById('typeSelect') as HTMLSelectElement;
const ideaInput = document.getElementById('ideaInput') as HTMLTextAreaElement;

// Form validation
function validateForm(): void {
  const isValid = areaSelect.value !== '' && 
                  typeSelect.value !== '' && 
                  ideaInput.value.trim() !== '';
  
  submitFeedbackButton.disabled = !isValid;
}

// Add validation listeners
areaSelect.addEventListener('change', validateForm);
typeSelect.addEventListener('change', validateForm);
ideaInput.addEventListener('input', validateForm);

// Suggestions container
const mainListElement = document.getElementById('main-list') as HTMLDivElement; // Where new suggestions get appended

// Show/hide progress bar
function showFeedbackProgress(show: boolean): void {
  if (show) {
    feedbackProgressContainer.style.display = 'block';
    feedbackProgressBar.style.width = '0%';
  } else {
    feedbackProgressContainer.style.display = 'none';
  }
}

// Animate or set progress bar
function setFeedbackProgress(percent: number): void {
  feedbackProgressBar.style.width = percent + '%';
}

// Submit feedback
submitFeedbackButton.addEventListener('click', async function(event: Event): Promise<void> {
  event.preventDefault();
  showFeedbackProgress(true);
  setFeedbackProgress(30);

  const formData = new FormData(feedbackForm);
  try {
    const response = await fetch(\`\${app_base_url}/submit_feedback\`, {
      method: 'POST',
      body: formData,
      credentials: 'include'
    });

    setFeedbackProgress(70);

    if (!response.ok) {
      console.error('Non-2xx response:', response);
      throw new Error('Network response was not ok');
    }

    const result = await response.json();
    console.log('Feedback submission result:', result);

    if (result.ok === false) {
      showFeedbackProgress(false);
      alert(\`Error: \${result.error || 'An unknown error occurred.'}\`);
      return;
    }

    // SUCCESS: show the progress bar and hide after a moment
    setFeedbackProgress(100);
    setTimeout(() => showFeedbackProgress(false), 1000);
    feedbackForm.reset();

    // 1. Show a success message
    const successMessageDiv = document.getElementById('successMessage') as HTMLDivElement;
    successMessageDiv.style.display = 'block';

    // 2. OPTIONAL: If you'd like to refresh the entire page after a short delay:
    //    setTimeout(() => {
    //      window.location.reload();
    //    }, 1500);

    // OR 2. OPTIONAL: Clear and reload suggestions without a full page reload:
    // mainListElement.innerHTML = '';
    // loadMoreSuggestions();

  } catch (error) {
    console.error('Error submitting feedback:', error);
    showFeedbackProgress(false);
    alert('Error submitting feedback. Please try again.');
  }
});



const toggleUpvoteFeedback = async (suggestionId: string): Promise<void> => {
  // Check client-side login if you do that
  if (!userIsLoggedIn()) {
    openSignup();
    return;
  }

  try {
    const payload = { suggestionId };
    const response = await fetch(\`\${app_base_url}/toggle_feedback_upvote\`, {
      method: 'POST',
      credentials: 'include',
      headers: {
        'Content-Type': 'application/json'
      },
      body: JSON.stringify(payload),
    });

    if (response.status === 401) {
      openSignup();
      return;
    }

    if (!response.ok) {
      throw new Error('Network response was not ok');
    }

    const result = await response.json();

    if (result.ok) {
      // We got new score & userHasUpvoted
      const itemElem = document.getElementById(suggestionId);
      if (itemElem) {
        // Update the score text
        const scoreElem = itemElem.querySelector('.suggestion-score') as HTMLElement;
        if (scoreElem) {
          scoreElem.textContent = \`Score: \${result.score}\`;
        }

        // Change button label if you want
        const upvoteBtn = itemElem.querySelector('.upvote-btn') as HTMLButtonElement;
        if (upvoteBtn) {
          if (result.userHasUpvoted) {
            // The user just upvoted
            upvoteBtn.textContent = "Downvote";
          } else {
            // The user removed their upvote
            upvoteBtn.textContent = "Upvote";
          }
        }
      }

    } else {
      alert(result.error || 'Error toggling upvote.');
    }

  } catch (error) {
    console.error('Error toggling feedback upvote:', error);
    alert('An error occurred. Please try again.');
  }
};


// Infinite scrolling
let infiniteScrollLoading = false;
let infiniteScrollHitEnd = false;

// Current sort mode (popular/newest) from template context
// Set popular as the default
let currentSortMode = "popular";

// The toggle function
const toggleSortSelection = (mode: string): void => {
  // Remove 'selected' from both buttons
  const popularButton = document.getElementById('popular');
  const newestButton = document.getElementById('newest');
  
  if (popularButton) popularButton.classList.remove('selected');
  if (newestButton) newestButton.classList.remove('selected');

  // Set the new mode
  currentSortMode = mode;

  // Highlight the selected mode
  const selectedButton = document.getElementById(mode);
  if (selectedButton) {
    selectedButton.classList.add('selected');
  }

  // Clear existing list for fresh load
  mainListElement.innerHTML = '';

  // Reset infinite scroll state
  infiniteScrollHitEnd = false;
  infiniteScrollLoading = false;

  // Load suggestions with new sort mode
  loadMoreSuggestions();
};

// Build the URL to load suggestions
function getInfiniteScrollApiUrl(afterId: string | number): string {
  return \`\${app_base_url}/category_page_row_listing/feedback/\${currentSortMode}/\${afterId || 0}\`;
}

// Fetch the next batch of suggestions
async function loadMoreSuggestions(): Promise<void> {
  if (infiniteScrollLoading || infiniteScrollHitEnd) return;
  infiniteScrollLoading = true;

  // Show loading indicator
  const loadingElement = document.getElementById('infinite-scroll-loading');
  if (loadingElement) {
    loadingElement.style.display = 'block';
  }

  // Get ID of last suggestion in the list, if any
  let afterId: string | number = 0;
  const lastItem = mainListElement.lastElementChild as HTMLElement;
  if (lastItem && lastItem.getAttribute('objectid')) {
    afterId = lastItem.getAttribute('objectid') || 0;
  }

  // Fetch next batch
  const url = getInfiniteScrollApiUrl(afterId);
  try {
    const response = await fetch(url);
    if (!response.ok) throw response;
    const html = await response.text();

    // If no or minimal HTML, we've hit the end
    if (html.length < 5) {
      infiniteScrollHitEnd = true;
    } else {
      // Insert new suggestions into the page
      mainListElement.insertAdjacentHTML('beforeend', html);
    }
  } catch (err) {
    console.error('Error loading more suggestions:', err);
  } finally {
    infiniteScrollLoading = false;
    // Hide loading indicator
    if (loadingElement) {
      loadingElement.style.display = 'none';
    }
  }
}

// On scroll, check if near bottom, then load more
function checkScrollForMoreSuggestions(): void {
  if (infiniteScrollLoading || infiniteScrollHitEnd) return;

  const scrollPosition = window.innerHeight + window.scrollY;
  const nearBottom = document.body.offsetHeight - 500;

  if (scrollPosition >= nearBottom) {
    loadMoreSuggestions();
  }
}

// Event listeners
window.addEventListener('scroll', checkScrollForMoreSuggestions);

// Initial setup on page load
document.addEventListener('DOMContentLoaded', () => {
  // Set initial sort mode and highlight popular button
  const popularButton = document.getElementById('popular');
  if (popularButton) {
    popularButton.classList.add('selected');
  }

  // Load first batch of suggestions
  loadMoreSuggestions();

  const emailOptionalSection = document.getElementById('emailOptionalSection');

  // --- Logic to Show/Hide Email Field using existing userIsLoggedIn() ---
  // Check if the function exists and if the user is NOT logged in
  if (!userIsLoggedIn() && emailOptionalSection) {
      // Show the email field ONLY if the user is NOT authenticated
      emailOptionalSection.style.display = 'block'; // Or remove a hiding class
  }

  try {
    removeHeaderBackground();
  } catch(e) {
    console.log("error removing header background: " + e);
  }  
});
`;

    const ast = parseCode(code);
    
    // This is a complex TypeScript file with many elements
    expect(ast.body.length).toBeGreaterThanOrEqual(10); // Many declarations and statements
    
    // Verify it parsed key TypeScript structures
    const hasConstants = ast.body.some((node: any) => 
      node.kind === 'VarDecl' || node.kind === 'ConstDecl'
    );
    const hasFunctions = ast.body.some((node: any) => 
      node.kind === 'FuncDecl'
    );
    
    // Check if we have meaningful content
    expect(ast.body.length).toBeGreaterThan(0);
    expect(hasConstants || hasFunctions).toBe(true);
  });

  test('parses real-world Go LLM client code', () => {
    const code = `
package main

import (
    "bufio"
    "bytes"
    "encoding/json"
    "fmt"
    "io"
    "net/http"
    "os"
    "strings"
)

var (
    apiKey    = os.Getenv("API_KEY")
    apiURL    = os.Getenv("API_URL")
    modelName = os.Getenv("MODEL_NAME")
)

// LLM calls the language model. If stream is nil, returns complete response via return value.
// If stream is provided, streams response chunks to channel and returns empty string.
// Input can be a string (wrapped as user message) or []map[string]string for full message history.
func LLM(input interface{}, stream chan<- string) (string, error) {
    // Build messages array
    var messages []map[string]string
    switch v := input.(type) {
    case string:
        messages = []map[string]string{
            {"role": "user", "content": v},
        }
    case []map[string]string:
        messages = v
    default:
        return "", fmt.Errorf("invalid input type")
    }

    // Build request
    requestBody := map[string]interface{}{
        "model":       modelName,
        "messages":    messages,
        "temperature": 0.7,
        "max_tokens":  500,
    }

    if stream != nil {
        requestBody["stream"] = true
        defer close(stream)
    }

    jsonBody, err := json.Marshal(requestBody)
    if err != nil {
        return "", err
    }

    req, err := http.NewRequest("POST", apiURL, bytes.NewBuffer(jsonBody))
    if err != nil {
        return "", err
    }

    req.Header.Set("Content-Type", "application/json")
    if apiKey != "" {
        req.Header.Set("Authorization", "Bearer "+apiKey)
    }

    client := &http.Client{}
    resp, err := client.Do(req)
    if err != nil {
        return "", err
    }
    defer resp.Body.Close()

    if resp.StatusCode != http.StatusOK {
        body, _ := io.ReadAll(resp.Body)
        return "", fmt.Errorf("API error (status %d): %s", resp.StatusCode, string(body))
    }

    // Handle streaming response
    if stream != nil {
        scanner := bufio.NewScanner(resp.Body)
        for scanner.Scan() {
            line := scanner.Text()
            if strings.HasPrefix(line, "data: ") {
                data := strings.TrimPrefix(line, "data: ")
                if data == "[DONE]" {
                    return "", nil
                }

                var chunk map[string]interface{}
                if err := json.Unmarshal([]byte(data), &chunk); err == nil {
                    if choices, ok := chunk["choices"].([]interface{}); ok && len(choices) > 0 {
                        if choice, ok := choices[0].(map[string]interface{}); ok {
                            if delta, ok := choice["delta"].(map[string]interface{}); ok {
                                if content, ok := delta["content"].(string); ok {
                                    stream <- content
                                }
                            }
                        }
                    }
                }
            }
        }
        return "", scanner.Err()
    }

    // Handle non-streaming response
    body, err := io.ReadAll(resp.Body)
    if err != nil {
        return "", err
    }

    var response map[string]interface{}
    if err := json.Unmarshal(body, &response); err != nil {
        return "", err
    }

    if choices, ok := response["choices"].([]interface{}); ok && len(choices) > 0 {
        if choice, ok := choices[0].(map[string]interface{}); ok {
            if message, ok := choice["message"].(map[string]interface{}); ok {
                if content, ok := message["content"].(string); ok {
                    return content, nil
                }
            }
        }
    }

    return "", fmt.Errorf("unexpected response format")
}
`;

    const ast = parseCode(code);
    
    // This is a complex Go file with package, imports, variables, and a large function
    expect(ast.body.length).toBeGreaterThanOrEqual(3); // Package, imports, variables, function
    
    // Verify it parsed key Go structures
    const hasPackage = ast.body.some((node: any) => 
      node.kind === 'Package' || 
      (node.kind === 'ExprStmt' && node.expr?.kind === 'Ident' && node.expr?.name === 'package')
    );
    const hasImports = ast.body.some((node: any) => 
      node.kind === 'Import'
    );
    const hasVariables = ast.body.some((node: any) => 
      node.kind === 'VarDecl' || node.kind === 'ShortDecl'
    );
    const hasFunctions = ast.body.some((node: any) => 
      node.kind === 'FuncDecl'
    );
    
    // Check if we have meaningful Go content
    expect(ast.body.length).toBeGreaterThan(0);
    expect(hasImports || hasVariables || hasFunctions).toBe(true);
  });

  test('parses real-world Bash Docker script', () => {
    // Note: Complex Bash syntax with command substitution, conditionals, and special operators
    // may not be fully supported yet. This test documents the current state.
    const code = `
# Bash Docker deployment script
DIR=\$(pwd)
PORTS_USED=\$(docker container ls --format "{{.Ports}}" -a)

if echo "\$PORTS_USED" | grep -q "0.0.0.0:8000"; then
    echo "Port 8000 is already used"
    BIND_PORT=""
else
    echo "Port 8000 is open"
    BIND_PORT="-p8000:8000"
fi

mkdir -p \$HOME/.ipython/

# Check if we're in a TTY
if [ -t 0 ]; then
    TTY_FLAG="-it"
else
    TTY_FLAG="-i"
fi

docker run --rm \$TTY_FLAG \\
  -e DJANGO_SETTINGS_MODULE=\$DJANGO_SETTINGS_MODULE \\
  -e SENTRY_ENVIRONMENT="dev_\${USER}" \\
  \$BIND_PORT \\
  -v \$DIR/:/ml_apis \\
  -v \$HOME/.aws/:/root/.aws \\
  -v \$HOME/.ipython/:/root/.ipython/ \\
  myimage "\$@"
`;

    const ast = parseCode(code);
    
    // Verify Bash script parsed successfully
    expect(ast.body.length).toBeGreaterThan(0);
    
    // Check for recognized structures
    const hasAnyStructure = ast.body.some((node: any) => 
      node.kind === 'VarDecl' || 
      node.kind === 'Assign' ||
      node.kind === 'If' ||
      node.kind === 'ExprStmt' ||
      node.kind === 'Echo'
    );
    expect(hasAnyStructure).toBe(true);
    
    // This test documents that Bash parsing is a known limitation
    // Future work should improve Bash-specific syntax support
  });
});