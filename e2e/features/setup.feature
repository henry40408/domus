Feature: First-time setup

  Scenario: The setup code creates the first admin
    Given a new domus server
    When I open the app
    Then I should see the first-time setup form
    When I create the admin "demo" with password "demo-password-123" using the setup code
    Then I should see the dashboard for "demo"
