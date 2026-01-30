//! Integration tests for socket handoff.
//!
//! These tests require a Unix-like environment with socket support.

#[test]
fn test_handoff_data_parsing() {
    use serde::Deserialize;

    #[derive(Debug, Deserialize)]
    struct HandoffData {
        user_id: Option<i64>,
        prompt: Option<String>,
    }

    let json = r#"{"user_id": 123, "prompt": "Hello world"}"#;
    let data: HandoffData = serde_json::from_str(json).unwrap();

    assert_eq!(data.user_id, Some(123));
    assert_eq!(data.prompt, Some("Hello world".to_string()));
}
